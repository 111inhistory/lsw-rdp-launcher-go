package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

const helpColumnWidth = 25

func printRootHelp(out io.Writer, flags *flag.FlagSet) {
	w := tabwriter.NewWriter(out, helpColumnWidth, 4, 3, ' ', 0)
	fmt.Fprintln(w, versionString())
	fmt.Fprintln(w, `Usage: rdp-launcher [global options] COMMAND [options] [arguments]

Connections & applications
  desktop / remoteapp	Connect or switch to desktop / RemoteApp
  run	Start a Windows application
  open	Open files as one batch with one confirmation
  apps	Query applications in the connected Windows login
  mounts	Query mapped drives in the connected Windows login

Catalog & shortcuts
  sync-apps	Sync applications, icons and desktop entries
  install-shortcuts	Rebuild shortcuts from local cache; no VM wake
  list-apps	List cached and configured applications; no VM wake
  path	Convert Linux / Windows paths using [mounts]

Controller
  start	Start the user service
  status	Show status; no VM wake
  lock / unlock	Block / allow automatic lifecycle actions
  shutdown	Stop controller and disconnect RDP; no Windows logoff
  ping	Check Windows backend; may wake VM, but starts no RDP
  serve / watcher	Run controller in the foreground (debugging)

Credentials & configuration
  auth	Manage RDP password: set / check / clear
  auth agent	Manage Agent token: set / check / clear
  show-config	Print configuration
  version	Print version and commit

Global options (before COMMAND)`)
	_ = w.Flush()
	printFlagHelp(out, flags)
	fmt.Fprintln(out, `
Examples
  rdp-launcher desktop
  rdp-launcher run WINWORD.EXE
  rdp-launcher open '/data/shared/report.docx'
  rdp-launcher --on-conflict=current run WINWORD.EXE

Command options go before application or file arguments.
See run --help, open --help and path --help for command options.
Mode switches prompt by default; --on-conflict selects an action directly.`)
}

func commandFlags(name, syntax, description string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	f.Usage = func() {
		fmt.Fprintln(out, versionString())
		fmt.Fprintf(out, "Usage: rdp-launcher [global options] %s\n\n%s\n", syntax, description)
		hasFlags := false
		f.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(out, "\nOptions")
			printFlagHelp(out, f)
		}
	}
	return f
}

// Keep flag's parser and metadata, but use standard short/long option spelling
// consistently in every help page. Redirected help needs no ANSI styling.
func printFlagHelp(out io.Writer, flags *flag.FlagSet) {
	w := tabwriter.NewWriter(out, helpColumnWidth, 4, 3, ' ', 0)
	flags.VisitAll(func(f *flag.Flag) {
		prefix := "--"
		if utf8.RuneCountInString(f.Name) == 1 {
			prefix = "-"
		}
		value, description := flag.UnquoteUsage(f)
		option := prefix + f.Name
		if value != "" {
			option += " " + value
		}
		fmt.Fprintf(w, "  %s\t%s\n", option, description)
		if f.DefValue != "" && f.DefValue != "false" {
			def := f.DefValue
			if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(def, home+string(os.PathSeparator)) {
				def = "~" + strings.TrimPrefix(def, home)
			}
			fmt.Fprintf(w, "\tDefault: %s\n", def)
		}
	})
	_ = w.Flush()
}
