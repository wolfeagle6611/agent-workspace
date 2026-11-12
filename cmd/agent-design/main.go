// Command agent-design runs one store.
//
// `serve` exposes that store over the sync protocol and runs commands for a
// host workspace (docs/adr/0008-second-store-shape.md). Commands run inside a
// FUSE mount of the mirror, so they see real paths.
//
// The same binary is the peer. Wrapping it in a container is a launcher
// change, not a protocol change.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/mount"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: agent-design serve --db <path> --socket <path>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "agent-design:", err)
		os.Exit(1)
	}
}

// serve listens on a unix socket and answers the sync protocol.
func serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	db := flags.String("db", "", "path of the mirror database")
	socket := flags.String("socket", "", "unix socket to listen on")
	mountRoot := flags.String("mount-root", "", "directory mount points live in")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *db == "" || *socket == "" {
		return errors.New("both --db and --socket are required")
	}

	store, err := vfs.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()

	// A peer killed with SIGKILL leaves its socket behind. Removing it is what
	// makes a restart work without an operator.
	if err := os.Remove(*socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	root := *mountRoot
	if root == "" {
		root = mount.DefaultRoot()
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("mount root %q is not absolute", root)
	}
	id, err := mount.MountID(*db)
	if err != nil {
		return err
	}
	session := mount.NewSession(mount.NewVolume(store), mount.NewManager(root), id)
	defer func() { _ = session.Unmount(context.Background()) }()

	runner := peer.NewRunner(mount.Resolver(store, session))
	server := peer.NewServerWithRunner(store, runner)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = make(map[net.Conn]struct{})
	)
	defer wg.Wait()

	// Shutdown closes live connections. Waiting for an idle peer to hang up
	// on its own would keep a stopped process alive for as long as the host
	// keeps its socket open.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		mu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mu.Lock()
		conns[conn] = struct{}{}
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			defer func() {
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
			}()
			if err := server.Serve(ctx, conn); err != nil && ctx.Err() == nil {
				log.Printf("connection: %v", err)
			}
		}()
	}
}
