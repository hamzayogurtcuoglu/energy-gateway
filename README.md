# EEBUS Inverter Simulator

A Go PV-inverter simulator that runs as a real EEBUS device (SHIP/TLS + SPINE).
It publishes inverter measurements and accepts an active-power production limit
via the `CS-LPP` use case. A bundled Energy Guard client can write that limit
over real EEBUS.

## Components

| Command | Role |
| --- | --- |
| `cmd/eebus-inverter-simulator` | The inverter — EEBUS device, `CS-LPP` server |
| `cmd/eebus-energyguard` | Energy Guard client that writes a production limit (`EG-LPP`) |

## Run the inverter

```powershell
go run ./cmd/eebus-inverter-simulator -eebus-interface Wi-Fi
```

It listens on EEBUS port `4711` and prints its SKI:

```text
real EEBUS inverter service listening port=4711 ski=da8816c2...
```

Main flags: `-eebus-port` (4711), `-eebus-interface` (mDNS NIC, e.g. `Wi-Fi`),
`-remote-ski` (peer to trust), `-nominal-peak-w` (10000), `-battery-capacity-wh` (12000).

## Manipulate a value over EEBUS

Two EEBUS peers must trust each other's SKI, and the higher SKI initiates the
connection — so both SKIs are provided.

```powershell
# 1) Get the guard SKI (prints ski=..., stored in .energyguard/, then idles — stop it)
go run ./cmd/eebus-energyguard

# 2) Inverter, trusting the guard SKI
go run ./cmd/eebus-inverter-simulator -eebus-port 47711 -eebus-interface Wi-Fi -remote-ski <GUARD_SKI>

# 3) Guard, writing a 3000 W limit to the inverter
go run ./cmd/eebus-energyguard -eebus-port 47712 -eebus-interface Wi-Fi -inverter-ski <SIMULATOR_SKI> -limit-w 3000
```

Success: the inverter status log changes from `mode=normal ... limit=none` to
`mode=curtailed pvW=3000 ... limit=3000W`. Change `-limit-w` for a different value.

## Test with the official client (optional)

The official `enbility/eebus-go` `hems` example also connects and exchanges data:

```powershell
# generate its identity once — save the cert/key blocks to hems.crt / hems.key, note its SKI
go run github.com/enbility/eebus-go/cmd/hems@v0.7.0 4712

# then run both sides (two terminals)
go run ./cmd/eebus-inverter-simulator -eebus-interface Wi-Fi -remote-ski <HEMS_SKI>
go run github.com/enbility/eebus-go/cmd/hems@v0.7.0 4712 <SIMULATOR_SKI> hems.crt hems.key
```

## Notes

- SPINE measurements published: `acPowerTotal`, `acEnergyProduced`, `stateOfCharge`, `acVoltage`, `acFrequency`.
- `mdns: Failed to set multicast interface` and occasional `TLS handshake error ... EOF` are harmless.
- Discovery uses mDNS, so both peers must be on the same LAN. Different networks need an L2 VPN (e.g. ZeroTier) or an mDNS reflector.

## Layout

```text
cmd/eebus-inverter-simulator   inverter entrypoint + status logging
cmd/eebus-energyguard          energy guard client (writes production limit)
internal/eebus/service.go      EEBUS SHIP/TLS + SPINE implementation
internal/inverter/simulator.go inverter physics simulation
```

## Test

```powershell
go test ./...
```
