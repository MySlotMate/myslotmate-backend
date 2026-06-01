//go:build ignore

// Standalone debug script. Excluded from the regular build (the `ignore` tag)
// so the other scratch/*.go files can each declare their own `func main()`.
// Run with: go run scratch/check_bookings.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load(".env")
	dbURL := os.Getenv("DATABASE_URL")
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(context.Background())

	eventID := "ae77f5b3-4fdb-48a3-9006-37799d74091d"
	
	rows, err := conn.Query(context.Background(), 
		"SELECT occurrence_date, quantity FROM bookings WHERE event_id = $1", eventID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Printf("Bookings for event %s:\n", eventID)
	for rows.Next() {
		var date time.Time
		var qty int
		if err := rows.Scan(&date, &qty); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Date: %v | Qty: %d\n", date, qty)
	}
}
