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

	"filemill/internal/app"
	"filemill/internal/mailgun"
)

// version is the FileMill build version. Overridable at build time with
// -ldflags "-X main.version=$(git describe --tags)"; defaults to the tagged release.
var version = "0.1.1"

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
		if !once {
			mailLog := log.New(io.MultiWriter(os.Stderr, application.LogWriter()), "mailgun ", log.LstdFlags|log.LUTC)
			mail, err := mailgun.Load(root, application, mailLog)
			if err != nil {
				fatal(err)
			}
			if mail != nil {
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
		}
		runErr := application.Run(ctx, once)
		if server != nil {
			// Drain in-flight webhook requests on shutdown (Ctrl+C). Intake is
			// idempotent, so a request cut off here is safely retried by Mailgun.
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancelShutdown()
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
	fmt.Fprintf(os.Stderr, "Usage:\n  %s run [--once]\n  %s submit <operation> <file>\n  %s jobs get <job-id>\n  %s --version\n", name, name, name, name)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "filemill:", err); os.Exit(1) }
