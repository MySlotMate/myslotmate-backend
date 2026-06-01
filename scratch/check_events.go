//go:build ignore

// Standalone debug script. Excluded from the regular build (the `ignore` tag)
// so the other scratch/*.go files can each declare their own `func main()`.
// Run with: go run scratch/check_events.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load(".env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL not set")
	}

	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v\n", err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), "SELECT id, title, is_recurring, recurrence_rule, time FROM events ORDER BY created_at DESC LIMIT 5")
	if err != nil {
		log.Fatalf("Query failed: %v\n", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, title string
		var isRecurring bool
		var rule *string
		var t interface{}
		err := rows.Scan(&id, &title, &isRecurring, &rule, &t)
		if err != nil {
			log.Fatal(err)
		}
		ruleStr := "nil"
		if rule != nil {
			ruleStr = *rule
		}
		fmt.Printf("ID: %s | Title: %s | Recurring: %v | Rule: %s | Time: %v\n", id, title, isRecurring, ruleStr, t)
	}
}
