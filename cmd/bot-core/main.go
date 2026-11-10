package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"github.com/ZhenForest/xsbot-core/internal/admin"
	"github.com/ZhenForest/xsbot-core/internal/app"
	"github.com/ZhenForest/xsbot-core/internal/config"
	run "github.com/ZhenForest/xsbot-core/internal/runtime"
	"github.com/ZhenForest/xsbot-core/internal/service"
)

func main() { os.Exit(execute()) }
func execute() int {
	command := "run"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	directory := flags.String("dir", ".", "Core runtime directory")
	if e := flags.Parse(args); e != nil {
		return 2
	}
	root, e := filepath.Abs(*directory)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	if command == "check-config" {
		if _, e = config.Read(filepath.Join(root, "config.yaml")); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		return 0
	}
	budget := service.Open(root)
	if command == "service-check" {
		if e = budget.Condition(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		return 0
	}
	if command != "run" {
		if e = os.MkdirAll(filepath.Join(root, "data/runtime"), 0700); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		lock, e := run.AcquireLock(filepath.Join(root, "data/runtime/core.lock"))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		defer lock.Close()
		switch command {
		case "reset-budget":
			e = budget.Reset()
		case "reset-password":
			var c config.Config
			c, e = config.Read(filepath.Join(root, "config.yaml"))
			if e == nil {
				var password string
				password, e = admin.ResetIdentity(root, c.Web.InitialUsername)
				if e == nil {
					fmt.Println("Temporary Core password:", password)
				}
			}
		case "rebuild-certificate":
			var c config.Config
			c, e = config.Read(filepath.Join(root, "config.yaml"))
			if e == nil {
				_, _, e = admin.Certificate(root, c.Web.PublicIP, true)
			}
		default:
			e = fmt.Errorf("unknown command")
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		return 0
	}
	core, e := app.Load(root)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ready := func() error {
		if os.Getenv("NOTIFY_SOCKET") == "" {
			return nil
		}
		return budget.Ready()
	}
	code := core.Run(ctx, ready)
	if code == 75 && os.Getenv("NOTIFY_SOCKET") != "" {
		if e = budget.ManualRestart(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 0
		}
	}
	return code
}
