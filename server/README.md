# NanoKVM Server

This is the backend server implementation for NanoKVM.

For detailed documentation, please visit our [Wiki](https://wiki.sipeed.com/nanokvm).

## Structure

```shell
server
├── assets       // Embedded static assets (CSS, JS, images)
├── config       // Server configuration
├── logger       // Logging system
├── middleware   // Server middleware components
├── proto        // API request/response definitions
├── router       // API route handlers
├── service      // Core service implementations
├── templates    // Go templ page templates
├── utils        // Utility functions
└── main.go
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
    1. Run `cd server` from the project root directory.
    2. Run `go mod tidy` to install Go dependencies.
    3. Run `CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build` to compile the project.
    4. After compilation, an executable file named `NanoKVM-Server` will be generated.

2. Deploy the Application
    1. Enable SSH in the Web Settings if it is off: `Settings > SSH`. The BMC runs no sshd —
       the app itself is the SSH server, authenticating with the BMC account or a key from
       `Settings > Authorized Keys`. It serves shells and remote commands only: there is no
       SFTP subsystem and no `scp` binary on the device, so copy files over the HTTP API
       (`/api/firmware/file/:name`, `/api/firmware/media/upload`) or the web UI instead.
    2. Place the newly compiled `NanoKVM-Server` at `/var/lib/nanokvm/app/server/NanoKVM-Server` (the factory copy under the read-only `/kvmapp` is left untouched; the launcher prefers the writable install).
    3. Restart the service by running `killall NanoKVM-Server` — busybox init supervises the server and respawns it immediately.

## Manually Update

> File transfers use the HTTP API or the web UI, not `scp`/`sftp` — the in-process SSH
> server provides shells and remote commands only.

1. Download the latest application from [GitHub](https://github.com/sipeed/NanoKVM/releases);
2. Unzip the downloaded file and rename the unzipped folder to `kvmapp`;
3. Place it at `/var/lib/nanokvm/app` on your NanoKVM (`/kvmapp` itself is the read-only factory copy inside the squashfs; the launcher runs `/var/lib/nanokvm/app` first when present);
4. Run `killall NanoKVM-Server` to restart the service — busybox init respawns it from the new install.
