package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"flint/internal/app"
	"flint/internal/mcp"
	"flint/internal/store"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		os.Exit(app.RunCLI(nil, nil, os.Stdout, os.Stderr))
	}

	cmd := os.Args[1]

	if cmd == "init" {
		os.Exit(app.RunInitOrProfile(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}

	if cmd == "migrate" {
		os.Exit(app.RunMigrate(os.Args[2:], os.Stdout, os.Stderr))
	}

	dbPath, err := app.DefaultDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	repo, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db error: %v\n", err)
		os.Exit(1)
	}
	defer repo.Close()
	behaviorDBPath := filepath.Join(filepath.Dir(dbPath), "behavior.db")
	behaviorRepo, err := store.NewBehaviorSQLiteStore(behaviorDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "behavior db error: %v\n", err)
		os.Exit(1)
	}
	defer behaviorRepo.Close()

	svc := app.NewService(repo, behaviorRepo)

	// Resolve active project at boot and inject into service so all
	// read/write paths are automatically scoped (tasks 2.4–2.6).
	if activeProj, err := repo.GetActive(); err == nil {
		svc.SetActiveProject(activeProj.ID)
	}

	if cmd == "mcp" {
		fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		transport := fs.String("transport", "stdio", "mcp transport: stdio|sse")
		port := fs.String("port", "7878", "sse listen port")
		bind := fs.String("bind", "127.0.0.1", "sse bind address")
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}

		srv := mcp.NewServer(os.Stdin, os.Stdout, svc)
		if *transport == "sse" {
			if err := srv.RunSSE(*port, *bind); err != nil {
				fmt.Fprintf(os.Stderr, "mcp sse error: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if err := srv.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	os.Exit(app.RunCLIWithStdin(svc, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
