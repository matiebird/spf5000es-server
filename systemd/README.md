# systemd setup

The installer builds the server and runs it as a systemd service.

## Install

Install Go and `usbreset`, then run from the repository root:

```sh
sudo ./systemd/install-systemd.sh
```

Edit the generated configuration and restart the service:

```sh
sudoedit /etc/spf5000es-server/config.ini
sudo systemctl restart spf5000es-server.service
```

Running the installer again updates the server without replacing your
configuration.

## Useful commands

```sh
# Check the service
systemctl status spf5000es-server.service

# Follow its logs
journalctl -u spf5000es-server.service -f

# Install without starting the service
sudo ./systemd/install-systemd.sh --no-start
```

## Remote install

To build locally and install over SSH:

```sh
./systemd/install-systemd.sh pi@raspberrypi.local
```

The remote user must have `sudo` access. The remote machine also needs systemd,
`usbreset`, and the `dialout` group.
