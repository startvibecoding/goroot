package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

func cmdClient(args []string) int {
	if len(args) == 0 {
		clientUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		return clientRun(rest)
	case "ps", "list", "ls":
		return clientPs()
	case "logs", "log":
		return clientLogs(rest)
	case "stop", "kill":
		return clientStop(rest, false)
	case "rm", "remove":
		return clientRm(rest)
	case "status", "ping":
		return clientStatus()
	case "shutdown", "down":
		return clientShutdown()
	case "-h", "--help", "help":
		clientUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "goroot: client: unknown subcommand %q\n\n", sub)
		clientUsage()
		return 2
	}
}

func clientUsage() {
	fmt.Fprint(os.Stderr, `goroot client - talk to the goroot daemon

Usage:
  goroot client run [run-options] [--] <command> [args...]
  goroot client ps
  goroot client logs [-f] <id>
  goroot client stop <id>
  goroot client rm <id>
  goroot client status
  goroot client shutdown

Notes:
  * The daemon is started automatically if it is not running.
  * 'client run' accepts the same options as 'goroot run'.
`)
}

// mustCall dials the server, performs one request and returns the response.
func mustCall(req Request) *Response {
	c, err := dialServer()
	if err != nil {
		fatal("%v", err)
	}
	defer c.Close()
	resp, err := call(c, req)
	if err != nil {
		fatal("%v", err)
	}
	return resp
}

func clientRun(args []string) int {
	spec := buildRunSpec(args)
	// Resolve a relative rootfs against the client's cwd, since the daemon
	// runs elsewhere.
	if spec.Rootfs != "" {
		if abs, err := filepath.Abs(spec.Rootfs); err == nil {
			spec.Rootfs = abs
		}
	}
	c, err := dialServer()
	if err != nil {
		fatal("%v", err)
	}
	defer c.Close()

	resp, err := call(c, Request{Op: opRun, Spec: spec})
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println(resp.ID)
	return 0
}

func clientPs() int {
	resp := mustCall(Request{Op: opPs})
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPID\tSTATE\tEXIT\tNET\tSTARTED\tCOMMAND")
	for _, t := range resp.Tasks {
		exit := "-"
		if t.State == "exited" {
			exit = strconv.Itoa(t.Exit)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.PID, t.State, exit, t.Net, t.Started,
			strings.Join(t.Argv, " "))
	}
	w.Flush()
	return 0
}

func clientLogs(args []string) int {
	follow := false
	id := ""
	for _, a := range args {
		switch a {
		case "-f", "--follow":
			follow = true
		default:
			if strings.HasPrefix(a, "-") {
				fatal("logs: unknown flag %q", a)
			}
			id = a
		}
	}
	if id == "" {
		fatal("logs: need a task id (see `goroot client ps`)")
	}

	c, err := dialServer()
	if err != nil {
		fatal("%v", err)
	}
	defer c.Close()

	_, br, err := callRaw(c, Request{Op: opLogs, ID: id, Follow: follow})
	if err != nil {
		fatal("%v", err)
	}
	_, _ = io.Copy(os.Stdout, br)
	return 0
}

func clientStop(args []string, force bool) int {
	if len(args) < 1 {
		fatal("stop: need a task id")
	}
	resp := mustCall(Request{Op: opStop, ID: args[0]})
	fmt.Println(resp.Message)
	return 0
}

func clientRm(args []string) int {
	if len(args) < 1 {
		fatal("rm: need a task id")
	}
	resp := mustCall(Request{Op: opRm, ID: args[0]})
	fmt.Println(resp.Message)
	return 0
}

func clientStatus() int {
	resp := mustCall(Request{Op: opStatus})
	fmt.Println(resp.Message)
	return 0
}

func clientShutdown() int {
	resp := mustCall(Request{Op: opShutdown})
	fmt.Println(resp.Message)
	return 0
}
