// Command eebus-gateway is a small protocol-routing gateway.
//
// It exposes one HTTP API (POST /limit, GET /status) and routes each request to
// a backend device by its "target": the EEBUS inverter (eg/lpp + ma/mpc) or a
// Modbus device (Modbus TCP). The Matter side never needs to know which field
// protocol is used.
//
//	Matter node --HTTP--> this gateway --(EEBUS | Modbus)--> device
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type gateway struct {
	logger   *slog.Logger
	backends map[string]backend
}

func main() {
	eebusPort := flag.Int("eebus-port", 47712, "EEBUS SHIP listen port")
	ifaces := flag.String("eebus-interface", "", "network interface names for mDNS, e.g. Wi-Fi")
	inverterSKI := flag.String("inverter-ski", "", "SKI of the EEBUS inverter to control")
	modbusURL := flag.String("modbus-url", "tcp://127.0.0.1:5502", "Modbus TCP URL of the Modbus device")
	httpAddr := flag.String("http", "127.0.0.1:8090", "local bridge API address the Matter node calls")
	certPath := flag.String("cert", ".gateway/gateway.crt", "TLS certificate path")
	keyPath := flag.String("key", ".gateway/gateway.key", "TLS private key path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	eebusBE, ski, err := newEEBUSBackend(logger, *eebusPort, splitCSV(*ifaces), *inverterSKI, *certPath, *keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eebus backend error: %v\n", err)
		os.Exit(1)
	}
	defer eebusBE.shutdown()

	modbusBE, err := newModbusBackend(logger, *modbusURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "modbus backend error: %v\n", err)
		os.Exit(1)
	}
	defer modbusBE.close()

	g := &gateway{
		logger: logger,
		backends: map[string]backend{
			"inverter": eebusBE,  // EEBUS device
			"modbus":   modbusBE, // Modbus device
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /limit", g.handleLimit)
	mux.HandleFunc("GET /status", g.handleStatus)
	httpServer := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("multi-protocol gateway running", "ski", ski, "eebusPort", *eebusPort, "modbusURL", *modbusURL, "http", *httpAddr, "targets", "inverter(eebus),modbus")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "http server failed: %v\n", err)
		os.Exit(1)
	}
	logger.Info("gateway shutting down")
}

func (g *gateway) handleLimit(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req limitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	target := req.Target
	if target == "" {
		target = "inverter"
	}
	be, ok := g.backends[target]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown target %q (known: inverter, modbus)", target), http.StatusBadRequest)
		return
	}

	var err error
	if req.Reset {
		err = be.Reset()
	} else {
		err = be.SetLimit(req.Watts, time.Duration(req.DurationSeconds)*time.Second)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	g.logger.Info("routed limit command", "target", target, "watts", req.Watts, "reset", req.Reset)
	g.writeStatus(w)
}

func (g *gateway) handleStatus(w http.ResponseWriter, _ *http.Request) {
	g.writeStatus(w)
}

// writeStatus returns the status of every backend keyed by target, e.g.
// {"inverter": {...}, "modbus": {...}}.
func (g *gateway) writeStatus(w http.ResponseWriter) {
	out := make(map[string]backendStatus, len(g.backends))
	for name, be := range g.backends {
		out[name] = be.Status()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
