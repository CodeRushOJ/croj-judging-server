package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
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
	var pepper []byte
	if commandMatches(os.Args[1:], "api-key", "create") {
		encodedPepper := os.Getenv("JUDGE_API_KEY_PEPPER_B64")
		decoded, err := base64.StdEncoding.DecodeString(encodedPepper)
		if err != nil || len(decoded) < 32 {
			return fmt.Errorf("JUDGE_API_KEY_PEPPER_B64 must encode at least 32 bytes")
		}
		pepper = decoded
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
	options, err := callbackProvisionerOptions(os.Args[1:], os.Getenv, rand.Reader)
	if err != nil {
		return err
	}
	provisioner, err := external.NewProvisioner(database, rand.Reader, options...)
	if err != nil {
		return err
	}
	return admincli.Run(ctx, os.Args[1:], provisioner, pepper, os.Stdout)
}

func callbackProvisionerOptions(arguments []string, getenv func(string) string, random io.Reader) ([]external.ProvisionerOption, error) {
	if !commandMatches(arguments, "callback", "create") {
		return nil, nil
	}
	callbackCipher, err := external.DecodeCallbackKeyRing(
		getenv("JUDGE_CALLBACK_KEY_VERSION"),
		getenv("JUDGE_CALLBACK_KEYS_JSON"),
		random,
	)
	if err != nil {
		return nil, err
	}
	return []external.ProvisionerOption{external.WithCallbackCipher(callbackCipher)}, nil
}

func commandMatches(arguments []string, resource, action string) bool {
	return len(arguments) >= 2 && arguments[0] == resource && arguments[1] == action
}
