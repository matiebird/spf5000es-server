# systemd setup

The installer builds the server and runs it as a systemd service.

## Install

Install Go, polkit, and `usbreset`, then run:

```sh
sudo ./systemd/install-systemd.sh
```

Edit the generated configuration and start the service:

```sh
sudoedit /etc/spf5000es-server/config.ini
sudo systemctl enable --now spf5000es-server.service
```

The installer leaves the service stopped by default so the configuration can
be reviewed first. Pass `--start` to enable and start it immediately.

Reinstalling preserves the configuration and `/usr/local/bin/spf5000es-recovery`.
Replace the latter to customize hardware recovery.

## Uninstall

```sh
sudo ./systemd/uninstall-systemd.sh
```

To uninstall from another machine over SSH:

```sh
./systemd/uninstall-systemd.sh pi@raspberrypi.local
```

The remote user needs `sudo`.

The uninstaller keeps all configuration files under
`/etc/spf5000es-server` intact and reports that they were not removed. It also
keeps `/usr/local/bin/spf5000es-recovery` because that program may have been
customized.

## Useful commands

```sh
# Check the service
systemctl status spf5000es-server.service

# Follow its logs
journalctl -u spf5000es-server.service -f

# Install and start immediately
sudo ./systemd/install-systemd.sh --start
```

## Remote install

To build locally and install over SSH:

```sh
./systemd/install-systemd.sh pi@raspberrypi.local
```

The remote user needs `sudo`; the host needs systemd, polkit, `usbreset`, and
the `dialout` group.
