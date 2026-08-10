// fw_env is a pure-Go replacement for U-Boot's fw_printenv / fw_setenv tools.
//
// With the preboot model U-Boot uses three plain-text files in the FAT
// partition:
//
//   - machine.env    — written by U-Boot every boot; full effective env
//   - persistent.env — applied on every boot (write here for continuous overrides)
//   - once.env       — applied on the next boot then deleted by U-Boot
//
// printenv/dump default to reading machine.env. setenv/script default to
// writing persistent.env. Use -f to override.
//
// Usage:
//
//	fw_env printenv [-f env_file] [-n] [name ...]
//	fw_env setenv   [-f env_file] name [value ...]
//	fw_env script   [-f env_file] script_file
//	fw_env dump     [-f env_file]
//
// If invoked as "fw_printenv" (symlink), it behaves like "fw_env printenv".
// If invoked as "fw_setenv"  (symlink), it behaves like "fw_env setenv".
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/ubootenv"
)

const (
	defaultMachineEnv    = "/var/lib/nanokvm/firmware/files/machine.env"
	defaultPersistentEnv = "/var/lib/nanokvm/firmware/files/persistent.env"
)

func main() {
	// Support symlink-based invocation (fw_printenv / fw_setenv).
	base := filepath.Base(os.Args[0])
	switch base {
	case "fw_printenv":
		os.Args = append([]string{os.Args[0], "printenv"}, os.Args[1:]...)
	case "fw_setenv":
		os.Args = append([]string{os.Args[0], "setenv"}, os.Args[1:]...)
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "printenv":
		cmdPrintenv(os.Args[2:])
	case "setenv":
		cmdSetenv(os.Args[2:])
	case "script":
		cmdScript(os.Args[2:])
	case "dump":
		cmdDump(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "fw_env: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: fw_env <command> [options] [args]

Commands:
  printenv [-f file] [-n] [name ...]   Print environment variables
  setenv   [-f file] name [value ...]  Set or delete a variable
  script   [-f file] script_file       Apply a batch script
  dump     [-f file]                   Dump raw key=value pairs

Options:
  -f file   Path to a U-Boot env file. Defaults:
              printenv/dump → `+defaultMachineEnv+`
              setenv/script → `+defaultPersistentEnv+`
  -n        Print value only (printenv, single var)

Symlink support:
  If invoked as "fw_printenv", acts as "fw_env printenv".
  If invoked as "fw_setenv",  acts as "fw_env setenv".

Script syntax:
  key [value]       Set key to value (spaces in value preserved)
  key               Delete key (no value)
  # comment         Lines starting with # are ignored`)
}

// cmdPrintenv implements fw_printenv.
func cmdPrintenv(args []string) {
	fs := flag.NewFlagSet("printenv", flag.ExitOnError)
	envFile := fs.String("f", defaultMachineEnv, "path to U-Boot env file")
	valueOnly := fs.Bool("n", false, "print value only (requires exactly one name)")
	fs.Parse(args)

	if *valueOnly && fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "fw_env printenv: -n requires exactly one variable name")
		os.Exit(1)
	}

	env, err := ubootenv.LoadFile(*envFile)
	if err != nil {
		fatal("open env", err)
	}

	names := fs.Args()
	if len(names) == 0 {
		printAll(env)
		return
	}

	exitCode := 0
	for _, name := range names {
		v, ok := env.Get(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "## Error: %q not defined\n", name)
			exitCode = 1
			continue
		}
		if *valueOnly {
			fmt.Println(v)
		} else {
			fmt.Printf("%s=%s\n", name, v)
		}
	}
	os.Exit(exitCode)
}

// printAll prints all variables sorted by key.
func printAll(env *ubootenv.Env) {
	keys := make([]string, 0, len(env.Vars))
	for k := range env.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, env.Vars[k])
	}
}

// cmdSetenv implements fw_setenv. By default it writes to persistent.env,
// loading the existing file (or starting from empty if it doesn't exist) so
// repeated invocations accumulate overrides.
func cmdSetenv(args []string) {
	fs := flag.NewFlagSet("setenv", flag.ExitOnError)
	envFile := fs.String("f", defaultPersistentEnv, "path to U-Boot env file")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "fw_env setenv: variable name required")
		os.Exit(1)
	}

	env, err := loadOrEmpty(*envFile)
	if err != nil {
		fatal("open env", err)
	}

	name := fs.Arg(0)
	if fs.NArg() == 1 {
		// Delete variable.
		env.Delete(name)
	} else {
		// Set variable: remaining args joined with spaces.
		value := strings.Join(fs.Args()[1:], " ")
		env.Set(name, value)
	}

	if err := env.SaveFile(*envFile); err != nil {
		fatal("save env", err)
	}
}

// cmdScript implements fw_parse_script.
func cmdScript(args []string) {
	fs := flag.NewFlagSet("script", flag.ExitOnError)
	envFile := fs.String("f", defaultPersistentEnv, "path to U-Boot env file")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "fw_env script: script file path required")
		os.Exit(1)
	}

	env, err := loadOrEmpty(*envFile)
	if err != nil {
		fatal("open env", err)
	}

	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fatal("open script", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		// Skip blank lines and comments.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Split into key and optional value.
		// "key" alone means delete; "key value..." means set.
		key, value, hasValue := strings.Cut(trimmed, " ")
		if hasValue {
			value = strings.TrimLeft(value, " \t")
			env.Set(key, value)
		} else {
			env.Delete(key)
		}
	}

	if err := scanner.Err(); err != nil {
		fatal("read script", err)
	}

	if err := env.SaveFile(*envFile); err != nil {
		fatal("save env", err)
	}

	fmt.Fprintf(os.Stderr, "fw_env: applied %d lines from script\n", lineNo)
}

// cmdDump prints all raw key=value pairs without formatting.
func cmdDump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	envFile := fs.String("f", defaultMachineEnv, "path to U-Boot env file")
	fs.Parse(args)

	env, err := ubootenv.LoadFile(*envFile)
	if err != nil {
		fatal("open env", err)
	}

	fmt.Fprintf(os.Stderr, "## Variables: %d\n", len(env.Vars))
	printAll(env)
}

// loadOrEmpty loads the env file, returning an empty Env if it does not exist.
// Useful for setenv/script targeting persistent.env or once.env which may not
// have been created yet.
func loadOrEmpty(path string) (*ubootenv.Env, error) {
	env, err := ubootenv.LoadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ubootenv.New(), nil
		}
		return nil, err
	}
	return env, nil
}

func fatal(context string, err error) {
	fmt.Fprintf(os.Stderr, "fw_env: %s: %v\n", context, err)
	os.Exit(1)
}
