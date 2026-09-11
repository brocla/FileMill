package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"filemill/internal/alert"
	"filemill/internal/app"
	"filemill/internal/mailgun"
)

// version is the FileMill build version, stamped at build time by
// scripts/Build-FileMill.ps1 via -ldflags "-X main.version=$(git describe)".
// A binary built directly with `go build`, bypassing that script, keeps this
// default — so an unstamped build is obviously identifiable rather than
// silently claiming a specific version number that only goes stale, the way
// a hand-maintained default here already has once.
var version = "dev"

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Println("filemill " + version)
		return
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	application, err := app.Open(root)
	if err != nil {
		fatal(err)
	}
	defer application.Close()

	switch os.Args[1] {
	case "submit":
		if len(os.Args) != 4 {
			usage()
			os.Exit(2)
		}
		id, err := application.Submit(os.Args[2], os.Args[3])
		if err != nil {
			fatal(err)
		}
		fmt.Println(id)
	case "jobs":
		if len(os.Args) != 4 || os.Args[2] != "get" {
			usage()
			os.Exit(2)
		}
		job, err := application.Job(os.Args[3])
		if err != nil {
			fatal(err)
		}
		fmt.Printf("id: %s\noperation: %s\nstatus: %s\nmessage: %s\n", job.ID, job.Operation, job.Status, job.Message)
	case "alert-test":
		// Sends one alert to alert_recipient, so the channel is proven (and
		// its spam placement known) before anything relies on it.
		if len(os.Args) != 2 {
			usage()
			os.Exit(2)
		}
		mail, err := mailgun.Load(root, application, log.New(os.Stderr, "mailgun ", log.LstdFlags|log.LUTC))
		if err != nil {
			fatal(err)
		}
		if mail == nil {
			fatal(fmt.Errorf("alert-test sends through Mailgun: set MAILGUN_API_KEY, MAILGUN_WEBHOOK_SIGNING_KEY, MAILGUN_DOMAIN, and REPLY_FROM"))
		}
		to := mail.AlertRecipient()
		if to == "" {
			fatal(fmt.Errorf("no alert_recipient in config/email.yaml"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := alert.SendTest(ctx, mail, application, to, time.Now()); err != nil {
			fatal(err)
		}
		fmt.Printf("test alert sent to %s; check that it arrives and isn't marked as spam\n", to)
	case "run":
		once := len(os.Args) == 3 && os.Args[2] == "--once"
		if len(os.Args) > 2 && !once {
			usage()
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// A webhook server that cannot listen takes the whole worker down with
		// it, so cancelling this context stops the job loop too.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var server *http.Server
		serverErrs := make(chan error, 1)
		// The alert Emailer runs on a context of its own, stopped only after the
		// job loop and the webhook server have finished, so an alert raised
		// while they shut down still goes out. alertsDone closes once it has
		// drained.
		alertCtx, stopAlerts := context.WithCancel(context.Background())
		defer stopAlerts()
		var alertsDone chan struct{}
		// reporter takes run's own restart alert: the Emailer once it is wired,
		// otherwise nothing.
		var reporter alert.Reporter = alert.Nop{}
		if !once {
			mailLog := log.New(io.MultiWriter(os.Stderr, application.LogWriter()), "mailgun ", log.LstdFlags|log.LUTC)
			mail, err := mailgun.Load(root, application, mailLog)
			if err != nil {
				fatal(err)
			}
			if mail != nil {
				// Operator alerts go out through the Mailgun adapter, from
				// REPLY_FROM, so they exist only when it does. The reporters are
				// set before any goroutine starts, so nothing reports into the
				// no-op default by accident.
				var emailer *alert.Emailer
				if to := mail.AlertRecipient(); to != "" {
					alertLog := log.New(io.MultiWriter(os.Stderr, application.LogWriter()), "alert ", log.LstdFlags|log.LUTC)
					emailer = alert.NewEmailer(mail, application, to, mail.AlertConfig(), time.Now, alertLog)
					application.SetReporter(emailer)
					mail.SetReporter(emailer)
					reporter = emailer
				}
				server = &http.Server{Addr: os.Getenv("LISTEN_ADDR"), Handler: mail.Handler()}
				if server.Addr == "" {
					server.Addr = ":8080"
				}
				// Bind before announcing anything. A worker that cannot accept
				// webhooks is not degraded, it is useless — but it would keep
				// running its delivery loop against the shared database, which
				// is how two workers once ran at once after a port collision.
				// Job claiming survives that; delivery has no such guard. So
				// the bind happens here, synchronously, and its failure ends
				// the process before a single goroutine is started — leaving
				// the supervisor's backoff to decide when to try again.
				//
				// Binding first also keeps the startup line honest: it is
				// printed only once the port is actually held.
				listener, err := net.Listen("tcp", server.Addr)
				if err != nil {
					fatal(fmt.Errorf("webhook server: %w", err))
				}
				mailLog.Printf("FileMill %s — webhook listening on %s; delivery loop started", version, server.Addr)
				if emailer != nil {
					alertsDone = make(chan struct{})
					go func() { emailer.Run(alertCtx); close(alertsDone) }()
					mailLog.Printf("operator alerts: sent to %s", mail.AlertRecipient())
				} else {
					mailLog.Print("operator alerts: disabled (no alert_recipient in email.yaml)")
				}
				go func() {
					if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
						mailLog.Printf("server: %v", err)
						serverErrs <- err
						cancel()
					}
				}()
				go mail.Deliver(ctx)
				// Retention only matters for published files, and the sweep is
				// inert when no route publishes any.
				go mail.SweepExpired(ctx)
			} else {
				mailLog.Print("integration disabled: no Mailgun environment variables set")
			}
			// Job workspaces accumulate whether or not email is configured, so
			// this sweep runs unconditionally in continuous mode rather than
			// nested under the mailgun branch above.
			go application.SweepExpiredJobs(ctx)

			// Only a starting worker may conclude that a job left running is
			// orphaned, and only the continuous one: it holds the webhook port
			// by now, so a second worker that lost the bind has already exited
			// without touching the first one's jobs. --once must never do this
			// — it binds nothing, and `run --once` alongside the real worker
			// would mark that worker's live job interrupted, which the delivery
			// loop takes as finished. Other commands never do it either.
			interrupted, err := application.InterruptLeftoverJobs()
			if err != nil {
				fatal(err)
			}
			if restart, ok := restartAlert(os.Getenv(previousExitEnv), os.Getenv(rapidRestartsEnv), interrupted); ok {
				reporter.Report(restart)
			}
		}
		runErr := application.Run(ctx, once)
		if server != nil {
			// Drain in-flight webhook requests on shutdown (Ctrl+C). Intake is
			// idempotent, so a request cut off here is safely retried by Mailgun.
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancelShutdown()
		}
		if alertsDone != nil {
			// Everything that reports has stopped. The Emailer sends what is
			// still queued, for up to 5 seconds, then returns.
			stopAlerts()
			<-alertsDone
		}
		// Checked before runErr: when the listener is what failed, Run returns
		// nil (it stopped because its context was cancelled), and reporting a
		// clean stop would hide the actual cause.
		select {
		case err := <-serverErrs:
			fatal(fmt.Errorf("webhook server: %w", err))
		default:
		}
		if runErr != nil {
			fatal(runErr)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage:\n  %s run [--once]\n  %s submit <operation> <file>\n  %s jobs get <job-id>\n  %s alert-test\n  %s --version\n", name, name, name, name, name)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "filemill:", err); os.Exit(1) }
