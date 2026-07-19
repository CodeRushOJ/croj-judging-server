package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/admincli"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	_ "github.com/go-sql-driver/mysql"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "judge-admin:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("JUDGE_DATABASE_DSN")
	if dsn == "" {
		return fmt.Errorf("JUDGE_DATABASE_DSN is required")
	}
	encodedPepper := os.Getenv("JUDGE_API_KEY_PEPPER_B64")
	pepper, err := base64.StdEncoding.DecodeString(encodedPepper)
	if err != nil || len(pepper) < 32 {
		return fmt.Errorf("JUDGE_API_KEY_PEPPER_B64 must encode at least 32 bytes")
	}

	database, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open judge database: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(5 * time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := database.PingContext(startupContext); err != nil {
		return fmt.Errorf("connect judge database: %w", err)
	}
	if err := external.ApplyMigrations(startupContext, database); err != nil {
		return err
	}
	provisioner, err := external.NewProvisioner(database, rand.Reader)
	if err != nil {
		return err
	}
	return admincli.Run(ctx, os.Args[1:], provisioner, pepper, os.Stdout)
}
