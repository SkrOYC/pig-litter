package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type collaborationCLIOptions struct {
	repo, root, catalog, scenario, report, target  string
	pig, extension, piglet                         string
	baselineTarget, baselinePig, baselineExtension string
	checkout, baselineCheckout                     string
	ffmpeg, ffprobe, xvfb, xterm                   string
	requireParity                                  bool
	performancePairs, performanceWarmups           int
}

type collaborationCLITarget struct {
	Identity  collaborationTarget `json:"identity"`
	Pig       string              `json:"pig"`
	Extension string              `json:"extension"`
	Checkout  string              `json:"checkout"`
}

type collaborationCLIScenario struct {
	purpose  string
	channel  string
	caseName string
}

var collaborationCLIScenarios = map[string]collaborationCLIScenario{
	"baseline":                         {purpose: "verification", channel: "live"},
	"startup-standalone":               {purpose: "verification", channel: "live", caseName: "direct"},
	"startup-source":                   {purpose: "verification", channel: "live", caseName: "source"},
	"startup-piglet":                   {purpose: "verification", channel: "live", caseName: "piglet"},
	"startup-unselected":               {purpose: "verification", channel: "live", caseName: "unselected"},
	"host-tool-veto":                   {purpose: "research", channel: "research"},
	"host-busy-message":                {purpose: "research", channel: "research"},
	"host-idle-message":                {purpose: "research", channel: "research"},
	"host-crash-after-acceptance":      {purpose: "research", channel: "research"},
	"host-crash-after-append":          {purpose: "research", channel: "research"},
	"host-crash-after-acknowledgement": {purpose: "research", channel: "research"},
	"host-same-id-replay":              {purpose: "research", channel: "research"},
	"host-independent-process":         {purpose: "research", channel: "research"},
	"capture-performance":              {purpose: "verification", channel: "performance"},
	"fixture-isolation":                {purpose: "verification", channel: "live"},
	"fixture-interrupt":                {purpose: "verification", channel: "live"},
}

func collaborationMain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 3 && args[0] == "--crash-worker" {
		if err := collaborationRunCrashWorker(ctx, args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(args) >= 3 && args[0] == "--probe-child" {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		var evidence any
		var err error
		switch {
		case len(args) == 3 && (args[1] == "busy" || args[1] == "idle"):
			evidence, err = collaborationRunHostMessageProbe(ctx, args[2], args[1])
		case len(args) == 3 && collaborationCrashStage(args[1]):
			evidence, err = collaborationRunHostCrashProbe(ctx, args[2], args[1])
		case len(args) == 3 && args[1] == "registration":
			evidence, err = collaborationRunHostRegistrationProbe(ctx, args[2])
		case len(args) == 5 && args[1] == "independent":
			evidence, err = collaborationRunHostIndependentProcessProbe(ctx, args[2], args[3], args[4])
		default:
			fmt.Fprintln(os.Stderr, "invalid private probe arguments")
			return 2
		}
		_ = json.NewEncoder(os.Stdout).Encode(evidence)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(args) == 2 && args[0] == "--hook-child" {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		evidence, err := collaborationRunHostHookProbe(ctx, args[1])
		_ = json.NewEncoder(os.Stdout).Encode(evidence)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	options, err := collaborationParseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err == nil {
		if options.report != "" {
			err = collaborationCLIReport(ctx, options)
		} else {
			err = collaborationCLIRun(ctx, options, args)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func collaborationParseOptions(args []string) (collaborationCLIOptions, error) {
	var options collaborationCLIOptions
	flags := flag.NewFlagSet("collaboration", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Select a baseline scenario or report over an explicit evidence selection.")
		flags.PrintDefaults()
	}
	flags.StringVar(&options.repo, "repo-root", "", "repository root")
	flags.StringVar(&options.root, "evidence-root", "", "absolute evidence directory")
	flags.StringVar(&options.catalog, "catalog", "", "catalog snapshot")
	flags.StringVar(&options.scenario, "scenario", "", "scenario name")
	flags.StringVar(&options.report, "report", "", "immutable evidence selection")
	flags.StringVar(&options.target, "target", "", "target identity JSON")
	flags.StringVar(&options.pig, "pig-bin", "", "selected PiG executable")
	flags.StringVar(&options.extension, "extension", "", "selected extension executable")
	flags.StringVar(&options.checkout, "target-checkout", "", "clean checkout for the selected product commit")
	flags.StringVar(&options.piglet, "piglet", "", "selected Piglet manifest")
	flags.StringVar(&options.baselineTarget, "baseline-target", "", "baseline target identity JSON")
	flags.StringVar(&options.baselinePig, "baseline-pig-bin", "", "baseline PiG executable")
	flags.StringVar(&options.baselineExtension, "baseline-extension", "", "baseline extension executable")
	flags.StringVar(&options.baselineCheckout, "baseline-checkout", "", "clean checkout for the baseline product commit")
	flags.StringVar(&options.ffmpeg, "media-ffmpeg", "ffmpeg", "FFmpeg executable")
	flags.StringVar(&options.ffprobe, "media-ffprobe", "ffprobe", "FFprobe executable")
	flags.StringVar(&options.xvfb, "media-xvfb", "Xvfb", "Xvfb executable")
	flags.StringVar(&options.xterm, "media-xterm", "xterm", "xterm executable")
	flags.BoolVar(&options.requireParity, "require-parity", false, "fail unless parity is measured")
	flags.IntVar(&options.performancePairs, "performance-pairs", 20, "measured interleaved pairs per capture mode")
	flags.IntVar(&options.performanceWarmups, "performance-warmups", 5, "warmup pairs per capture mode")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 || (options.scenario == "") == (options.report == "") {
		return options, errors.New("select exactly one of --scenario NAME or --report SELECTION")
	}
	if !filepath.IsAbs(options.repo) || !filepath.IsAbs(options.root) || !filepath.IsAbs(options.target) {
		return options, errors.New("--repo-root, --evidence-root, and --target must be absolute paths")
	}
	if options.catalog == "" {
		options.catalog = filepath.Join(options.repo, "scripts", "testdata", "collaboration", "requirements.json")
	}
	if !filepath.IsAbs(options.catalog) {
		return options, errors.New("--catalog must be an absolute path")
	}
	if options.piglet == "" {
		options.piglet = filepath.Join(options.repo, "piglet.yaml")
	}
	if options.report != "" && !filepath.IsAbs(options.report) {
		return options, errors.New("--report must be an absolute path")
	}
	if options.performancePairs < 1 || options.performancePairs > 20 || options.performanceWarmups < 0 || options.performanceWarmups > 5 {
		return options, errors.New("performance requires 1 to 20 pairs and 0 to 5 warmups")
	}
	return options, nil
}

func collaborationReadTarget(path string) (collaborationTarget, error) {
	var target collaborationTarget
	data, err := os.ReadFile(path)
	if err == nil {
		err = collaborationDecodeJSON(data, &target)
	}
	if err == nil {
		err = target.validate()
	}
	return target, err
}

func collaborationCLIReport(ctx context.Context, options collaborationCLIOptions) error {
	target, err := collaborationReadTarget(options.target)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	selection, err := os.ReadFile(options.report)
	if err != nil {
		return err
	}
	catalog, err := os.ReadFile(options.catalog)
	if err != nil {
		return err
	}
	_, evidence, err := collaborationSelectEvidence(ctx, options.root, selection, catalog, target, collaborationMediaTools{FFmpeg: options.ffmpeg, FFprobe: options.ffprobe})
	if err != nil {
		return err
	}
	report, err := collaborationReduceReport(evidence, collaborationProofRegistry{})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	if options.requireParity && !report.Parity {
		return errors.New("parity is not established by the selected evidence")
	}
	return nil
}

func collaborationCLIDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func collaborationCLICheckTarget(target collaborationCLITarget) error {
	for _, entry := range []struct{ label, path, digest string }{
		{"PiG host", target.Pig, target.Identity.HostBinarySHA256},
		{"extension", target.Extension, target.Identity.ProductBinarySHA256},
	} {
		if !filepath.IsAbs(entry.path) {
			return fmt.Errorf("%s path must be absolute", entry.label)
		}
		digest, err := collaborationCLIDigest(entry.path)
		if err != nil {
			return fmt.Errorf("hash %s at %s: %w", entry.label, entry.path, err)
		}
		if digest != entry.digest {
			return fmt.Errorf("%s digest mismatch for %s", entry.label, entry.path)
		}
	}
	return nil
}

func collaborationCLIWriteNew(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}
