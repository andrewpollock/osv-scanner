package helper

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/google/osv-scanner/v2/internal/spdx"
	"github.com/google/osv-scanner/v2/pkg/osvscanner"
	"github.com/google/osv-scanner/v2/pkg/reporter"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

// OfflineFlags is a map of flags which require network access to operate,
// with the values to set them to in order to disable them
var OfflineFlags = map[string]string{
	"include-git-root":                     "true",
	"experimental-offline-vulnerabilities": "true",
	"experimental-no-resolve":              "true",
	"experimental-licenses-summary":        "false",
	// "experimental-licenses": "", // StringSliceFlag has to be manually cleared.
}

// sets default port(8000) as a global variable
var (
	servePort = "8000" // default port
)

var GlobalScanFlags = []cli.Flag{
	&cli.StringFlag{
		Name:      "config",
		Usage:     "set/override config file",
		TakesFile: true,
	},
	&cli.StringFlag{
		Name:    "format",
		Aliases: []string{"f"},
		Usage:   "sets the output format; value can be: " + strings.Join(reporter.Format(), ", "),
		Value:   "table",
		Action: func(_ *cli.Context, s string) error {
			if slices.Contains(reporter.Format(), s) {
				return nil
			}

			return fmt.Errorf("unsupported output format \"%s\" - must be one of: %s", s, strings.Join(reporter.Format(), ", "))
		},
	},
	&cli.BoolFlag{
		Name:  "serve",
		Usage: "output as HTML result and serve it locally",
	},
	&cli.StringFlag{
		Name:  "port",
		Usage: "port number to use when serving HTML report (default: 8000)",
		Action: func(_ *cli.Context, p string) error {
			servePort = p
			return nil
		},
	},
	&cli.StringFlag{
		Name:      "output",
		Usage:     "saves the result to the given file path",
		TakesFile: true,
	},
	&cli.StringFlag{
		Name:  "verbosity",
		Usage: "specify the level of information that should be provided during runtime; value can be: " + strings.Join(reporter.VerbosityLevels(), ", "),
		Value: "info",
	},
	&cli.BoolFlag{
		Name:  "experimental-offline",
		Usage: "run in offline mode, disabling any features requiring network access",
		Action: func(ctx *cli.Context, b bool) error {
			if !b {
				return nil
			}
			// Disable the features requiring network access.
			for flag, value := range OfflineFlags {
				// TODO(michaelkedar): do something if the flag was already explicitly set.

				// Skip setting the flag if the current command doesn't have it."
				if !slices.ContainsFunc(ctx.Command.Flags, func(f cli.Flag) bool {
					return slices.Contains(f.Names(), flag)
				}) {
					continue
				}

				if err := ctx.Set(flag, value); err != nil {
					panic(fmt.Sprintf("failed setting offline flag %s to %s: %v", flag, value, err))
				}
			}

			return nil
		},
	},
	&cli.BoolFlag{
		Name:  "experimental-offline-vulnerabilities",
		Usage: "checks for vulnerabilities using local databases that are already cached",
	},
	&cli.BoolFlag{
		Name:  "experimental-download-offline-databases",
		Usage: "downloads vulnerability databases for offline comparison",
	},
	&cli.BoolFlag{
		Name:  "experimental-no-resolve",
		Usage: "disable transitive dependency resolution of manifest files",
	},
	&cli.StringFlag{
		Name:   "experimental-local-db-path",
		Usage:  "sets the path that local databases should be stored",
		Hidden: true,
	},
	&cli.BoolFlag{
		Name:  "experimental-all-packages",
		Usage: "when json output is selected, prints all packages",
	},
	&cli.BoolFlag{
		Name:  "experimental-licenses-summary",
		Usage: "report a license summary, implying the --experimental-all-packages flag",
	},
	&cli.StringSliceFlag{
		Name:  "experimental-licenses",
		Usage: "report on licenses based on an allowlist",
	},
}

// OpenHTML will attempt to open the outputted HTML file in the default browser
func OpenHTML(r reporter.Reporter, outputPath string) {
	// Open the outputted HTML file in the default browser.
	r.Infof("Opening %s...\n", outputPath)
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", outputPath).Start()
	case "windows":
		err = exec.Command("start", "", outputPath).Start()
	case "darwin": // macOS
		err = exec.Command("open", outputPath).Start()
	default:
		r.Infof("Unsupported OS.\n")
	}

	if err != nil {
		r.Errorf("Failed to open: %s.\n Please manually open the outputted HTML file: %s\n", err, outputPath)
	}
}

// ServeHTML serves the single HTML file for remote accessing.
// The program will keep running to serve the HTML report on localhost
// until the user manually terminates it (e.g. using Ctrl+C).
func ServeHTML(r reporter.Reporter, outputPath string) {
	localhostURL := fmt.Sprintf("http://localhost:%s/", servePort)
	r.Infof("Serving HTML report at %s.\nIf you are accessing remotely, use the following SSH command:\n`ssh -L local_port:destination_server_ip:%s ssh_server_hostname`\n", localhostURL, servePort)
	server := &http.Server{
		Addr: ":" + servePort,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, outputPath)
		}),
		ReadHeaderTimeout: 3 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		r.Errorf("Failed to start server: %v\n", err)
	}
}

func GetScanLicensesAllowlist(context *cli.Context) ([]string, error) {
	if context.Bool("experimental-licenses-summary") && context.IsSet("experimental-licenses") {
		return nil, errors.New("--experimental-licenses-summary and --experimental-licenses flags cannot be set")
	}
	allowlist := context.StringSlice("experimental-licenses")
	if context.IsSet("experimental-licenses") {
		if len(allowlist) == 0 ||
			(len(allowlist) == 1 && allowlist[0] == "") {
			return nil, errors.New("--experimental-licenses requires at least one value")
		}
		if unrecognized := spdx.Unrecognized(allowlist); len(unrecognized) > 0 {
			return nil, fmt.Errorf("--experimental-licenses requires comma-separated spdx licenses. The following license(s) are not recognized as spdx: %s", strings.Join(unrecognized, ","))
		}
	}

	scanLicensesAllowlist := context.StringSlice("experimental-licenses")
	if context.Bool("experimental-offline") {
		scanLicensesAllowlist = []string{}
	}

	return scanLicensesAllowlist, nil
}

func GetReporter(context *cli.Context, stdout, stderr io.Writer, outputPath, format string) (reporter.Reporter, error) {
	termWidth := 0
	var err error
	if outputPath != "" { // Output is definitely a file
		stdout, err = os.Create(outputPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create output file: %w", err)
		}
	} else { // Output might be a terminal
		if stdoutAsFile, ok := stdout.(*os.File); ok {
			termWidth, _, err = term.GetSize(int(stdoutAsFile.Fd()))
			if err != nil { // If output is not a terminal,
				termWidth = 0
			}
		}
	}

	verbosityLevel, err := reporter.ParseVerbosityLevel(context.String("verbosity"))
	if err != nil {
		return nil, err
	}
	r, err := reporter.New(format, stdout, stderr, verbosityLevel, termWidth)
	if err != nil {
		return r, err
	}

	return r, nil
}

func GetExperimentalScannerActions(context *cli.Context, scanLicensesAllowlist []string) osvscanner.ExperimentalScannerActions {
	return osvscanner.ExperimentalScannerActions{
		LocalDBPath:       context.String("experimental-local-db-path"),
		DownloadDatabases: context.Bool("experimental-download-offline-databases"),
		CompareOffline:    context.Bool("experimental-offline-vulnerabilities"),
		// License summary mode causes all
		// packages to appear in the json as
		// every package has a license - even
		// if it's just the UNKNOWN license.
		ShowAllPackages: context.Bool("experimental-all-packages") ||
			context.Bool("experimental-licenses-summary"),
		ScanLicensesSummary:   context.Bool("experimental-licenses-summary"),
		ScanLicensesAllowlist: scanLicensesAllowlist,
	}
}
