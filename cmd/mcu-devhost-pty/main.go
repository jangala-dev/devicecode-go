//go:build !tinygo

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"devicecode-go/bus"
	"devicecode-go/services/fabric"
	"devicecode-go/services/updater"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mcu-devhost-pty: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	var cfg struct {
		uart           string
		stateDir       string
		node           string
		peer           string
		initialImageID string
		initialVersion string
		initialBuildID string
		rebootExitCode int
	}
	flag.StringVar(&cfg.uart, "uart", "", "POSIX tty path connected to the CM5-side PTY")
	flag.StringVar(&cfg.stateDir, "state-dir", "", "directory for devhost MCU state.json")
	flag.StringVar(&cfg.node, "node", "mcu", "local Fabric node id")
	flag.StringVar(&cfg.peer, "peer", "bigbox-cm5", "expected peer Fabric node id")
	flag.StringVar(&cfg.initialImageID, "initial-image-id", "mcu-dev-10.0", "initial running image id when state-dir is empty")
	flag.StringVar(&cfg.initialVersion, "initial-version", "10.0", "initial running version when state-dir is empty")
	flag.StringVar(&cfg.initialBuildID, "initial-build-id", "devhost-initial", "initial running build id when state-dir is empty")
	flag.IntVar(&cfg.rebootExitCode, "reboot-exit-code", 42, "exit code used to model MCU reboot after commit")
	flag.Parse()

	if cfg.uart == "" {
		return errors.New("--uart is required")
	}
	store, err := openStateStore(cfg.stateDir, imageState{ImageID: cfg.initialImageID, Version: cfg.initialVersion, BuildID: cfg.initialBuildID})
	if err != nil {
		return err
	}
	tr, err := openLineTransport(cfg.uart)
	if err != nil {
		return err
	}
	defer tr.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_ = updater.GenerateBootID()
	identity := store.identity()
	logJSON(map[string]any{
		"event": "ready", "node": cfg.node, "peer": cfg.peer,
		"image_id": identity.ImageID, "version": identity.Version, "build_id": identity.Build,
		"gomaxprocs": runtime.GOMAXPROCS(0),
	})

	b := bus.NewBus(64, "+", "#")
	updaterConn := b.NewConnection("updater")
	fabricConn := b.NewConnection("fabric")

	svc := updater.New(updater.Options{
		Conn:          updaterConn,
		Verifier:      devhostDCMCUVerifier{},
		Applier:       devhostApplier{store: store, exitCode: cfg.rebootExitCode},
		Metadata:      store,
		MetadataWrite: store,
		Identity:      identity,
	})
	go svc.Run(ctx)
	fabric.RunWithOptions(ctx, tr, fabricConn, cfg.node, cfg.peer, fabric.DefaultLinkConfig(), fabric.RunOptions{StageController: svc})
	return nil
}
