package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/kubewall/kubewall/backend/config"
	"github.com/kubewall/kubewall/backend/container"
	"github.com/kubewall/kubewall/backend/routes"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.PersistentFlags().String("certFile", "", "absolute path to certificate file")
	rootCmd.PersistentFlags().String("keyFile", "", "absolute path to key file")
	rootCmd.PersistentFlags().StringP("port", "p", ":7080", "port to listen on [deprecated, use --listen instead]")
	rootCmd.PersistentFlags().StringP("listen", "l", "[::]:7080", "IP and port to listen on (e.g., localhost:7080, :7080, or [::]:7080)")
	rootCmd.PersistentFlags().Int("k8s-client-qps", 100, "maximum QPS to the master from client")
	rootCmd.PersistentFlags().Int("k8s-client-burst", 200, "Maximum burst for throttle")
	rootCmd.PersistentFlags().Bool("no-open-browser", false, "Do not open the default browser")
	rootCmd.PersistentFlags().String("llm-api-endpoint", "", "LLM API endpoint URL")
	rootCmd.PersistentFlags().String("llm-api-key", "", "LLM API key (can also use KUBEWALL_LLM_API_KEY env var)")

}

var rootCmd = &cobra.Command{
	Use:   "kubewall",
	Short: "kubewall",
	Long:  `kubewall is a single binary web app to manage multiple clusters https://github.com/kubewall/kubewall`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return Serve(cmd)
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func Serve(cmd *cobra.Command) error {
	env := config.NewEnv()

	k8sClientQPS, err := cmd.Flags().GetInt("k8s-client-qps")
	if err != nil {
		return err
	}
	k9sClientBurst, err := cmd.Flags().GetInt("k8s-client-burst")
	if err != nil {
		return err
	}
	// Determine listen address
	listenAddr, err := cmd.Flags().GetString("listen")
	if err != nil {
		return err
	}
	// Backward compatibility: fallback to --port if --listen is not set
	port, err := cmd.Flags().GetString("port")
	if err != nil {
		return err
	}
	if port != ":7080" {
		log.Warn("Flag --port is deprecated, use --listen instead. This will be removed in a future release.")
		switch {
		case port == "":
			listenAddr = "localhost:7080" // default
		case port[0] == ':':
			listenAddr = "localhost" + port
		default:
			listenAddr = "localhost:" + port
		}
	}
	certFile, err := cmd.Flags().GetString("certFile")
	if err != nil {
		return err
	}
	keyFile, err := cmd.Flags().GetString("keyFile")
	if err != nil {
		return err
	}
	noOpen, err := cmd.Flags().GetBool("no-open-browser")
	if err != nil {
		return err
	}

	llmAPIEndpoint, err := cmd.Flags().GetString("llm-api-endpoint")
	if err != nil {
		return err
	}
	llmAPIKey, err := cmd.Flags().GetString("llm-api-key")
	if err != nil {
		return err
	}
	// Allow API key from environment variable if not provided via flag
	if llmAPIKey == "" {
		llmAPIKey = os.Getenv("KUBEWALL_LLM_API_KEY")
	}

	isSecure := certFile != "" || keyFile != ""

	cfg := config.NewAppConfig(Version, listenAddr, k8sClientQPS, k9sClientBurst, isSecure, llmAPIEndpoint, llmAPIKey)
	cfg.LoadAppConfig()

	c := container.NewContainer(env, cfg)
	e := echo.New()
	startBanner()
	routes.ConfigureRoutes(e, c)

	if !noOpen {
		openDefaultBrowser(c.Config().IsSecure, c.Config().ListenAddr)
	}

	if !isSecure && !strings.Contains(c.Config().ListenAddr, "[::]:7080") && !strings.Contains(c.Config().ListenAddr, "localhost") {
		log.Warn("SSE may not work properly without TLS. Use --certFile and --keyFile for HTTPS, or bind to localhost with --listen localhost:7080 to avoid issues.")
	}

	// Start server in a goroutine
	go func() {
		if c.Config().IsSecure {
			e.Pre(middleware.HTTPSRedirect())
			if err := e.StartTLS(c.Config().ListenAddr, certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Fatal("shutting down the server", "error", err)
			}
		} else {
			if err := e.Start(c.Config().ListenAddr); err != nil && err != http.ErrServerClosed {
				log.Fatal("shutting down the server", "error", err)
			}
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down gracefully...")

	// Shutdown all cluster informers
	shutdownAllClusters(c)

	// Stop event processor
	c.EventProcessor().Stop()

	// Shutdown server with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		log.Error("Server forced to shutdown", "error", err)
	}

	log.Info("Server exited")
	return nil
}

func shutdownAllClusters(c container.Container) {
	cfg := c.Config()
	for configName, kubeConfig := range cfg.KubeConfig {
		for clusterName, cluster := range kubeConfig.Clusters {
			log.Info("Shutting down cluster", "config", configName, "cluster", clusterName)
			cluster.Shutdown()
		}
	}
}

func openDefaultBrowser(isSecure bool, listenAddr string) {
	// Split IP and Port
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// fallback if listenAddr is invalid
		host = "localhost"
		port = "7080"
	}
	// Default to localhost if no IP is provided (e.g., ":7080")
	if host == "" || host == "::" {
		host = "localhost"
	}
	scheme := "http"
	if isSecure {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s:%s", scheme, host, port)
	// this will allow container apps to run
	browser.OpenURL(url)
}

func startBanner() {
	fmt.Println(" _          _                        _ _ ")
	fmt.Println("| | ___   _| |__   _____      ____ _| | |")
	fmt.Println("| |/ / | | | '_ \\ / _ \\ \\ /\\ / / _` | | |")
	fmt.Println("|   <| |_| | |_) |  __/\\ V  V / (_| | | |")
	fmt.Println("|_|\\_\\\\__,_|_.__/ \\___| \\_/\\_/ \\__,_|_|_|")
	fmt.Println("___________________________________________")
	fmt.Println("version:", Version)
	fmt.Println("commit:", Commit)
	fmt.Println("https://github.com/kubewall/kubewall")
	fmt.Println("___________________________________________")
}
