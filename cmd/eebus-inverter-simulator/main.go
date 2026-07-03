package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"eebus-inverter-simulator/internal/eebus"
	"eebus-inverter-simulator/internal/inverter"
)

func main() {
	eebusPort := flag.Int("eebus-port", 4711, "real EEBUS SHIP listen port")
	eebusInterfaces := flag.String("eebus-interface", "", "comma-separated network interface names for EEBUS mDNS, e.g. Wi-Fi")
	remoteSKI := flag.String("remote-ski", "", "optional remote EEBUS SKI to pair with")
	autoAccept := flag.Bool("eebus-auto-accept", true, "automatically accept incoming EEBUS pairing requests")
	certPath := flag.String("cert", ".eebus/inverter.crt", "EEBUS TLS certificate path")
	keyPath := flag.String("key", ".eebus/inverter.key", "EEBUS TLS private key path")
	serial := flag.String("serial", "SIM-INV-0001", "simulated device serial number")
	tick := flag.Duration("tick", time.Second, "simulation tick interval")
	nominalPeak := flag.Float64("nominal-peak-w", 10000, "nominal PV peak power in watts")
	batteryCapacity := flag.Float64("battery-capacity-wh", 12000, "battery capacity in watt-hours")
	initialSoC := flag.Float64("initial-soc", 55, "initial battery state of charge in percent")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	simulator := inverter.NewSimulator(inverter.Config{
		NominalPeakW:      *nominalPeak,
		BatteryCapacityWh: *batteryCapacity,
		InitialSoCPercent: *initialSoC,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go simulator.Run(ctx, *tick, time.Now)
	if *eebusPort <= 0 {
		fmt.Fprintln(os.Stderr, "eebus-port must be greater than 0")
		os.Exit(1)
	}

	realEEBUS, err := eebus.Start(ctx, simulator, eebus.Config{
		Port:           *eebusPort,
		RemoteSKI:      *remoteSKI,
		AutoAccept:     *autoAccept,
		Interfaces:     splitCSV(*eebusInterfaces),
		CertificatePEM: *certPath,
		PrivateKeyPEM:  *keyPath,
		Brand:          "SimGrid",
		Model:          "InverterSimulator",
		SerialNumber:   *serial,
		NominalPeakW:   *nominalPeak,
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start real EEBUS service: %v\n", err)
		os.Exit(1)
	}
	defer realEEBUS.Shutdown()

	go logStatus(ctx, simulator, logger)

	logger.Info("EEBUS inverter simulator running", "eebusPort", *eebusPort)
	<-ctx.Done()
	logger.Info("EEBUS inverter simulator shutting down")
}

// logStatus periodically prints the key inverter values so the effect of an
// EEBUS production limit is visible in the console.
func logStatus(ctx context.Context, simulator *inverter.Simulator, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state := simulator.Snapshot()
			limit := "none"
			if state.MaxActivePowerW != nil {
				limit = fmt.Sprintf("%.0fW", *state.MaxActivePowerW)
			}
			logger.Info("inverter status",
				"mode", state.OperationMode,
				"pvW", state.PVPowerW,
				"activeW", state.ActivePowerW,
				"socPct", state.BatterySoCPercent,
				"limit", limit)
		}
	}
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
