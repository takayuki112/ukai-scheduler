//go:build unix

// Command ukai-top shows users their budget, the tiers of their jobs and the queue of
// P1, and follows changes. It only reads, with the rights of the kubeconfig.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	kaiclient "github.com/kai-scheduler/KAI-scheduler/pkg/apis/client/clientset/versioned"

	"github.com/takayuki112/ukai-scheduler/pkg/dashboard"
)

const (
	settle = 200 * time.Millisecond // changes within this are drawn together
	tick   = 10 * time.Second       // only for the ages
)

func main() {
	// KAI's dependencies register flags of their own.
	flags := flag.NewFlagSet("ukai-top", flag.ExitOnError)
	var (
		user       = flags.String("user", "", "whose jobs (required)")
		arguments  = flags.String("arguments", "", "YAML file with the plugin's arguments, as for replay")
		configMap  = flags.String("configmap", "", "or the ConfigMap namespace/name with them, whose changes are followed")
		kubeconfig = flags.String("kubeconfig", "", "kubeconfig, if not the usual one")
		kubeCtx    = flags.String("context", "", "context, if not the current one")
		namespaces = flags.String("n", "", "comma-separated namespaces of your jobs; default: the context's")
		allPods    = flags.Bool("all-pods", false, "read the pods of all namespaces, if you may: exact units and GPU types of others")
		anonymous  = flags.Bool("anonymous", false, "do not show the names of other users and their jobs")
		limit      = flags.Int("limit", 10, "jobs of others to show in each queue")
		once       = flags.Bool("once", false, "print once, and exit; also if the output is not a terminal")
	)
	_ = flags.Parse(os.Args[1:]) // exits on errors
	if *user == "" || (*arguments == "") == (*configMap == "") || flags.NArg() > 0 {
		flags.Usage()
		os.Exit(2)
	}
	klog.LogToStderr(false) // it would draw over the screen
	klog.SetOutput(io.Discard)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	options := dashboard.Options{Others: !*anonymous, Limit: *limit}
	tty := term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
	if err := run(ctx, *user, *arguments, *configMap, *kubeconfig, *kubeCtx, *namespaces, *allPods, *once || !tty, options); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, dashboard.Clean(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, user, arguments, configMap, kubeconfig, kubeCtx, namespaces string, allPods, once bool, options dashboard.Options) error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeCtx})
	rest, err := loader.ClientConfig()
	if err != nil {
		return err
	}
	rest.UserAgent = "ukai-top"
	if namespaces == "" {
		if namespaces, _, err = loader.Namespace(); err != nil {
			return err
		}
	}
	var clients dashboard.Clients
	if clients.Kube, err = kubernetes.NewForConfig(rest); err != nil {
		return err
	}
	if clients.KAI, err = kaiclient.NewForConfig(rest); err != nil {
		return err
	}
	if clients.Metadata, err = metadata.NewForConfig(rest); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	settings, err := load(ctx, clients, arguments, configMap)
	if err != nil {
		return err
	}
	w, err := dashboard.Start(ctx, clients, settings, split(namespaces), allPods)
	if err != nil {
		return err
	}
	if !w.Synced(ctx) {
		return ctx.Err()
	}
	lines := func(width int) []string {
		o := options
		o.Width, o.Now = width, time.Now()
		config, _ := settings.Config()
		return dashboard.Render(dashboard.Compute(config, user, w.Data()), o)
	}
	if once {
		for _, l := range lines(0) {
			fmt.Println(l)
		}
		return nil
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	options.Color = !noColor && os.Getenv("TERM") != "dumb"
	return screen(ctx, w.Changed(), lines)
}

func load(ctx context.Context, clients dashboard.Clients, arguments, configMap string) (*dashboard.Settings, error) {
	if configMap != "" {
		namespace, name, ok := strings.Cut(configMap, "/")
		if !ok || namespace == "" || name == "" {
			return nil, fmt.Errorf("-configmap: %q is not namespace/name", configMap)
		}
		return dashboard.WatchSettings(ctx, clients.Kube, namespace, name)
	}
	text, err := os.ReadFile(arguments)
	if err != nil {
		return nil, err
	}
	config, err := dashboard.ParseSettings(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", arguments, err)
	}
	return dashboard.FixedSettings(config), nil
}

// split lists namespaces once each, as pods would otherwise count twice.
func split(list string) []string {
	items := strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' })
	slices.Sort(items)
	return slices.Compact(items)
}

// screen draws until a key or signal ends it, and always restores the terminal.
func screen(ctx context.Context, changed <-chan struct{}, lines func(width int) []string) error {
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	before, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l") // the alternate screen, without a cursor
	defer func() {
		fmt.Print("\x1b[0m\x1b[?25h\x1b[?1049l")
		_ = term.Restore(in, before)
	}()
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	keys := make(chan byte)
	go func() {
		buffer := make([]byte, 16)
		for {
			n, err := os.Stdin.Read(buffer)
			if err != nil {
				close(keys)
				return
			}
			select {
			case keys <- dashboard.Key(buffer[:n]):
			case <-ctx.Done():
				return
			}
		}
	}()
	clock := time.NewTicker(tick)
	defer clock.Stop()
	top := 0
	for {
		width, height, err := term.GetSize(out)
		if err != nil || width <= 0 || height <= 0 {
			width, height = 80, 24
		}
		var frame string
		frame, top = dashboard.Frame(lines(width), top, height)
		fmt.Print(frame)
		select {
		case <-ctx.Done():
			return nil
		case key, open := <-keys:
			if !open || key == 'q' || key == 3 { // Ctrl-C
				return nil
			}
			top = dashboard.Scroll(key, top, height)
		case <-resized:
		case <-clock.C:
		case <-changed:
			select {
			case <-time.After(settle):
			case <-ctx.Done():
				return nil
			}
		}
	}
}
