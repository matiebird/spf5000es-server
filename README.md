# SPF 5000 ES Server

A small Go service that reads a Growatt SPF 5000 ES inverter over Modbus and
publishes its status and configuration to MQTT. It also provides Home Assistant
MQTT discovery and accepts supported configuration commands through MQTT.

## Requirements

- Go 1.26.5 or newer
- A Growatt SPF 5000 ES connected through a serial/USB device
- An MQTT broker
- Linux with systemd, polkit, `usbreset`, and the `dialout` group for service installation

## Quick start

Copy the example configuration:

```sh
cp config.ini.example config.ini
```

Edit `config.ini` and set at least:

- `MODBUS.PORT` to the inverter's serial device, such as `/dev/ttyUSB0`
- `MQTT.HOST` to the MQTT broker address
- `MQTT.USER` and `MQTT.PASSWORD` if authentication is required

Run the server from the repository directory:

```sh
go run .
```

The server reads `config.ini` from its current working directory. Stop it with
`Ctrl+C`.

## Install as a systemd service

On the target Linux machine:

```sh
sudo ./systemd/install-systemd.sh
sudoedit /etc/spf5000es-server/config.ini
sudo systemctl restart spf5000es-server.service
```

Useful commands:

```sh
systemctl status spf5000es-server.service
journalctl -u spf5000es-server.service -f
```

To build locally and install on another Linux machine over SSH:

```sh
./systemd/install-systemd.sh user@hostname
```

See [systemd/README.md](systemd/README.md) for installation and removal details.

## MQTT topics

Topics use the configured `MQTT.TOPIC_PREFIX` (default:
`growatt/spf5000es`):

- `.../availability` reports `online` or `offline`
- `.../status/...` contains inverter readings
- `.../config/.../state` contains configuration values
- `.../config/.../set` accepts supported configuration changes
- `.../time_sync/set` requests an inverter clock sync

Home Assistant discovery messages use `MQTT.HA_DISCOVERY_PREFIX`, which defaults
to `homeassistant`.

## Development

Run the test suite:

```sh
go test ./...
```

Build the binary:

```sh
go build -o spf5000es-server .
```

This project is licensed under the terms in [LICENSE](LICENSE).
