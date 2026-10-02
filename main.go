package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"reconcore/internal/reconcore"
)

func main() {
	host := flag.String("host", "127.0.0.1", "interface to bind")
	port := flag.Int("port", 8080, "TCP port to bind")
	database := flag.String("database", "reconcore.db", "path of the JSON database file")
	functionalCurrency := flag.String("functional-currency", "CNY", "functional reporting currency")
	flag.Parse()

	store, err := reconcore.OpenStore(*database)
	if err != nil {
		fail(err)
	}
	service, err := reconcore.NewService(store, *functionalCurrency, nil)
	if err != nil {
		fail(err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		fail(err)
	}

	fmt.Printf("ReconCore listening on http://%s:%d\n", *host, *port)
	server := &http.Server{Handler: reconcore.NewServer(service)}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "reconcore: %s\n", err)
	os.Exit(1)
}
