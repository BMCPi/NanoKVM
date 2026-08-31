// bmc-sensord is the normal-world consumer of the OP-TEE BMC sensor pTA on
// the Raspberry Pi 5, a pure-Go port of docs/optee-sensor/bmc_sensord.c from
// the rpi5-uefi-build tree.
//
// Two modes, matching the original:
//
//	bmc-sensord --once           print the latest cached sample and exit
//	bmc-sensord [--period MS]    block on CMD_WAIT, printing each new sample
//
// The push to the BMC over I2C is autonomous inside OP-TEE; this daemon is
// only needed to re-run the RP1-BAR handshake if the OS moved the BAR
// (--init), and to observe or forward samples on the host side.
//
// Unlike the C original this needs no libteec: pkg/optee drives /dev/teeN by
// ioctl directly, so the binary cross-compiles for the Pi with CGO disabled.
// It runs on the managed host, not on the BMC.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/device/optee"
)

func main() {
	os.Exit(run())
}

// run returns the process exit status: 0 on success, 1 on failure, 2 on a
// usage error — the same three the C original uses.
func run() int {
	fs := flag.NewFlagSet("bmc-sensord", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(),
			"usage: %s [--once] [--init [--bar ADDR]] [--period MS]\n", os.Args[0])
		fs.PrintDefaults()
	}
	once := fs.Bool("once", false, "print the latest cached sample and exit")
	doInit := fs.Bool("init", false, "re-run the RP1-BAR handshake before reading")
	period := fs.Uint("period", 0, "sampling period in milliseconds, sent by --init")
	bar := hexUint64(RP1BARDefault)
	fs.Var(&bar, "bar", "RP1 BAR base address, for --init")

	if err := fs.Parse(os.Args[1:]); err != nil {
		// ContinueOnError has already printed the reason and the usage.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}

	client, err := Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer client.Close()

	if *doInit {
		cfg := DefaultConfig()
		cfg.BAR = uint64(bar)
		//nolint:gosec // a millisecond count that overflows int64 is not a period
		cfg.Period = time.Duration(*period) * time.Millisecond
		if err := client.Init(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	if *once {
		sample, err := client.Get()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(sample)
		return 0
	}

	return stream(client)
}

// stream blocks on the TA until each new sample arrives, printing it.
//
// The invoke blocks inside an ioctl, so a signal alone cannot end it: the
// handler asks the TEE to cancel the outstanding request, which returns it as
// ErrCancel and lets the loop unwind through the normal path (closing the
// session, releasing the device). Whether the TA honours cancellation is up
// to the TA, so the second signal is left to the default disposition and
// kills the process outright.
func stream(client *Client) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	cancelled := make(chan struct{})
	go func() {
		<-sigs
		close(cancelled)
		// Best effort: if the TA does not poll for cancellation the call
		// stays blocked, and the next signal terminates the process.
		signal.Stop(sigs)
		_ = client.Cancel()
	}()

	var lastSeq uint32
	for {
		sample, err := client.Wait(lastSeq, 0)
		if err != nil {
			select {
			case <-cancelled:
				// Expected: the signal handler asked for this.
				return 0
			default:
			}
			if optee.IsCancelled(err) {
				return 0
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		lastSeq = sample.Seq
		fmt.Println(sample)
	}
}

// hexUint64 is an address-valued flag. flag.Uint64 would accept the same
// 0x-prefixed input — its Set already parses in base 0, matching the C
// original's strtoull(…, 0) — but it renders the default in decimal, and
// "133143986176" is not a recognisable BAR. This keeps the round trip in the
// base an address is written in.
type hexUint64 uint64

func (h *hexUint64) Set(s string) error {
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", s, err)
	}
	*h = hexUint64(v)
	return nil
}

func (h *hexUint64) String() string { return "0x" + strconv.FormatUint(uint64(*h), 16) }
