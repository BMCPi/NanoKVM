# NanoKVM Server

This is the backend server implementation for NanoKVM.

For detailed documentation, please visit our [Wiki](https://wiki.sipeed.com/nanokvm).

## Structure

```shell
.
├── api          // HTTP API — one subpackage per sub-router (auth, application,
│                // vm, network, redfish, firmware, autoupdate), each holding
│                // its routes and gin service handlers
├── cmd/server   // Server entry point (composition: telemetry → ui → api)
├── pkg          // Domain + infrastructure packages: config, logger, middleware,
│                // proto, telemetry, utils, and one package per domain
│                // (ipmi, firmware, network, power, auth, …); pkg/redfish
│                // holds the shared OpenAPI spec
└── ui           // Web front-end: templ layouts/pages/components (incl. the
                 // vendored shadcn-templ library) + embedded static assets
```

## Configuration

The configuration file path is `/etc/kvm/server.yaml`.

```yaml
# Network Settings
proto: http            # Access protocol. Can be changed to `https` only when certificates are configured. Default is `http`
port:
    http: 80           # The listening port for the HTTP service. Default is `80`
    https: 443         # The listening port for the HTTPS service (effective when HTTPS is enabled). Default is `443`
cert:
    crt: server.crt    # The path to the public key certificate for HTTPS
    key: server.key    # The path to the private key file for HTTPS


# Logging Configuration
logger:
    level: info                          # Global log output level. Evaluated options from highest to lowest detail: `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`. Default is `info`
    file: /var/log/NanoKVM-Server.log    # Log output destination. A file path directs log output to that file, which is rotated automatically (10 MB per file, 3 compressed backups, 28-day retention). `console` outputs to stdout instead. Default is `/var/log/NanoKVM-Server.log`


# Authentication & Security
authentication: enable              # Whether to enable identity verification for HTTP API and Web endpoints. Options are `enable` or `disable`. Default is `enable`. Highly recommended to leave this enabled for internet-facing devices!
jwt:
   secretKey: ""                    # The secret key used to sign and verify JWT Tokens. If left empty, a random key will be generated automatically on startup
   refreshTokenDuration: 2678400    # The token refresh duration threshold in seconds before forcing a re-login. Default is `2678400` (~31 days)
   revokeTokensOnLogout: true       # Whether to invalidate all existing tokens upon logout by rotating the SecretKey. Default is `true`
security:
   loginLockoutDuration: 0,         # The duration (in seconds) to ban an IP from attempting to log in again after reaching the failure limit. If set to `0` or left empty, brute-force protection is disabled. Default is `0`
   loginMaxFailures:     5,         # The maximum number of continuous failed login attempts allowed per IP before triggering protection. Default is `5`
```

## Compile & Deploy

Note: Use Linux operating system (x86-64), or any platform with Go cross-compilation support.

1. Compile the Project
    1. Run `go mod tidy` from the project root directory to install Go dependencies.
    2. Run `CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -o NanoKVM-Server ./cmd/server` to compile the project.
    3. After compilation, an executable file named `NanoKVM-Server` will be generated.

2. Deploy the Application
    1. Enable SSH in the Web Settings if it is off: `Settings > SSH`. The BMC runs no sshd —
       the app itself is the SSH server, authenticating with the BMC account or a key from
       `Settings > Authorized Keys`. It serves shells, remote commands, and file transfer:
       the SFTP subsystem is served in-process, so `scp NanoKVM-Server root@<bmc>:/var/lib/nanokvm/app/server/`
       works even though the image ships no `sftp-server` or `scp` binary. Legacy `scp -O`
       does not — it needs an `scp` binary on the device — so drop the flag if a script sets it.
    2. Place the newly compiled `NanoKVM-Server` at `/var/lib/nanokvm/app/server/NanoKVM-Server`, overwriting what is there (the launcher seeds that path from the read-only factory copy under `/kvmapp` on first boot, and prefers it on every start thereafter). Keep it executable — `chmod 0755` after the copy if your transfer drops the mode, or the launcher will skip it and fall back to the factory build.
    3. Restart the service by running `killall NanoKVM-Server` — busybox init supervises the server and respawns it immediately.

## Manually Update

> File transfers can go over `scp`/`sftp` (served in-process by the SSH server), the HTTP
> API, or the web UI. Relative paths land in `/root`, the same place a shell session starts.

1. Download the latest application from [GitHub](https://github.com/sipeed/NanoKVM/releases);
2. Unzip the downloaded file and rename the unzipped folder to `kvmapp`;
3. Place it at `/var/lib/nanokvm/app` on your NanoKVM (`/kvmapp` itself is the read-only factory copy inside the squashfs, which the launcher seeds `/var/lib/nanokvm/app` from and then runs in preference to it);
4. Run `killall NanoKVM-Server` to restart the service — busybox init respawns it from the new install.
