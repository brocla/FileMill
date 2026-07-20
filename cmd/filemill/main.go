package main

import (
	"context"
	"fmt"
	"io"
	"log"
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
var version = "0.1.0"

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
		var server *http.Server
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
				mailLog.Printf("FileMill %s — webhook listening on %s; delivery loop started", version, server.Addr)
				go func() {
					if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						mailLog.Printf("server: %v", err)
					}
				}()
				go mail.Deliver(ctx)
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
