package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/openshift/check-payload/dist/releases"
	"github.com/openshift/check-payload/internal/scan"
	"github.com/openshift/check-payload/internal/types"
	"github.com/openshift/check-payload/internal/validations"
)

const (
	defaultPayloadFilename = "payload.json"
	defaultConfigFile      = "config.toml"
)

//go:embed config.toml
var embeddedConfig string

var applicationDeps = []string{
	"nm",
	"oc",
	"podman",
}

var applicationDepsNodeScan = []string{
	"nm",
	"rpm",
}

var Commit string

var (
	components                            []string
	configFile, configForVersion          string
	cpuProfile                            string
	failOnWarnings                        bool
	filterFiles, filterDirs, filterImages []string
	javaDisabledAlgorithms                []string
	insecurePull                          bool
	limit                                 int
	outputFile                            string
	outputFormat                          string
	parallelism                           int
	printExceptions                       bool
	pullSecretFile                        string
	timeLimit                             time.Duration
	verbose                               bool
)

func main() {
	var config types.Config
	var results []*types.ScanResults

	rootCmd := cobra.Command{
		Use:           "check-payload",
		SilenceErrors: true,
	}
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "verbose")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(Commit)
			return nil
		},
	}

	scanCmd := &cobra.Command{
		Use:   "scan",
		Short: "Run a scan",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := getConfig(&config.ConfigFile); err != nil {
				return err
			}
			config.FailOnWarnings = failOnWarnings
			config.FilterFiles = append(config.FilterFiles, filterFiles...)
			config.FilterDirs = append(config.FilterDirs, filterDirs...)
			config.FilterImages = append(config.FilterImages, filterImages...)
			config.Parallelism = parallelism
			config.InsecurePull = insecurePull
			config.OutputFile = outputFile
			config.OutputFormat = outputFormat
			config.PrintExceptions = printExceptions
			config.PullSecret = pullSecretFile
			config.Limit = limit
			config.TimeLimit = timeLimit
			config.Verbose = verbose
			config.Log()
			klog.InfoS("scan", "version", Commit)

			// Validate the configuration.
			err, warn := config.Validate()
			if warn != nil {
				klog.Warning(warn)
			}
			if err != nil {
				return fmt.Errorf("config has bad entries, please fix: %w", err)
			}

			if cpuProfile != "" {
				f, err := os.Create(cpuProfile)
				if err != nil {
					return err
				}
				if err := pprof.StartCPUProfile(f); err != nil {
					return err
				}
				klog.Info("collecting CPU profile data to ", cpuProfile)
			}

			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			if cpuProfile != "" {
				pprof.StopCPUProfile()
				klog.Info("CPU profile saved to ", cpuProfile)
			}
			scan.PrintResults(&config, results)
			if scan.IsFailed(results) {
				return errors.New("run failed")
			}
			if scan.IsWarnings(results) && config.FailOnWarnings {
				return errors.New("run failed with warnings")
			}
			return nil
		},
	}
	scanCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "use toml config file (default: "+defaultConfigFile+")")
	scanCmd.PersistentFlags().StringVarP(&configForVersion, "config-for-version", "V", "", "use embedded toml config file for specified version")
	scanCmd.PersistentFlags().StringSliceVar(&filterFiles, "filter-files", nil, "")
	scanCmd.PersistentFlags().StringSliceVar(&filterDirs, "filter-dirs", nil, "")
	scanCmd.PersistentFlags().StringSliceVar(&filterImages, "filter-images", nil, "")
	scanCmd.PersistentFlags().StringSliceVar(&components, "components", nil, "")
	scanCmd.PersistentFlags().BoolVar(&failOnWarnings, "fail-on-warnings", false, "fail on warnings")
	scanCmd.PersistentFlags().BoolVar(&insecurePull, "insecure-pull", false, "use insecure pull")
	scanCmd.PersistentFlags().IntVar(&limit, "limit", -1, "limit the number of pods scanned")
	scanCmd.PersistentFlags().IntVar(&parallelism, "parallelism", 5, "how many pods to check at once")
	scanCmd.PersistentFlags().StringVar(&outputFile, "output-file", "", "write report to file")
	scanCmd.PersistentFlags().StringVar(&outputFormat, "output-format", "table", "output format (table, csv, markdown, html)")
	scanCmd.PersistentFlags().StringVar(&pullSecretFile, "pull-secret", "", "pull secret to use for pulling images")
	scanCmd.PersistentFlags().DurationVar(&timeLimit, "time-limit", 1*time.Hour, "limit running time")
	scanCmd.PersistentFlags().StringVar(&cpuProfile, "cpuprofile", "", "write CPU profile to file")
	scanCmd.PersistentFlags().BoolVarP(&printExceptions, "print-exceptions", "p", false, "display exception list")

	scanPayload := &cobra.Command{
		Use:          "payload [image pull spec]",
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return scan.ValidateApplicationDependencies(applicationDeps)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
			defer cancel()
			config.FromURL, _ = cmd.Flags().GetString("url")
			config.FromFile, _ = cmd.Flags().GetString("file")
			if config.FromURL == "" && config.FromFile == "" {
				return errors.New("either -u, --url or -f, --file option is required")
			}
			config.PrintExceptions, _ = cmd.Flags().GetBool("print-exceptions")
			config.UseRPMScan, _ = cmd.Flags().GetBool("rpm-scan")
			results = scan.RunPayloadScan(ctx, &config)
			return nil
		},
	}
	scanPayload.Flags().StringP("url", "u", "", "payload url")
	scanPayload.Flags().StringP("file", "f", "", "payload from json file")
	scanPayload.MarkFlagsMutuallyExclusive("url", "file")
	scanPayload.Flags().Bool("rpm-scan", false, "use RPM scan (same as during node scan)")

	scanNode := &cobra.Command{
		Use:          "node --root /myroot [--walk-scan]",
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return scan.ValidateApplicationDependencies(applicationDepsNodeScan)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
			defer cancel()
			root, _ := cmd.Flags().GetString("root")
			walkScan, _ := cmd.Flags().GetBool("walk-scan")
			config.UseRPMScan = !walkScan
			results = scan.RunNodeScan(ctx, &config, root)
			return nil
		},
	}
	scanNode.Flags().String("root", "", "root path to scan")
	scanNode.Flags().Bool("walk-scan", false, "scan all files using directory tree walk")
	_ = scanNode.MarkFlagRequired("root")

	scanImage := &cobra.Command{
		Use:          "image [image pull spec]",
		Aliases:      []string{"operator"},
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return scan.ValidateApplicationDependencies(applicationDeps)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
			defer cancel()
			config.ContainerImage, _ = cmd.Flags().GetString("spec")
			config.UseRPMScan, _ = cmd.Flags().GetBool("rpm-scan")
			results = scan.RunOperatorScan(ctx, &config)
			return nil
		},
	}
	scanImage.Flags().String("spec", "", "payload url")
	scanImage.Flags().Bool("rpm-scan", false, "use RPM scan (same as during node scan)")
	_ = scanImage.MarkFlagRequired("spec")

	scanJavaImage := &cobra.Command{
		Use:          "java-image [image pull spec]",
		Aliases:      []string{"java"},
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return scan.ValidateApplicationDependencies(applicationDeps)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
			defer cancel()
			config.ContainerImage, _ = cmd.Flags().GetString("spec")
			config.UseRPMScan, _ = cmd.Flags().GetBool("rpm-scan")
			config.JavaDisabledAlgorithms = append(config.JavaDisabledAlgorithms, javaDisabledAlgorithms...)
			config.Java = true
			results = scan.RunOperatorScan(ctx, &config)
			return nil
		},
	}
	scanJavaImage.Flags().String("spec", "", "java payload url")
	scanJavaImage.Flags().Bool("rpm-scan", false, "use RPM scan (same as during node scan)")
	scanJavaImage.Flags().StringSliceVar(&javaDisabledAlgorithms, "disabled-algorithms", nil, "additional algorithms that java should be disabling in FIPS mode")
	_ = scanJavaImage.MarkFlagRequired("spec")

	gosyms := &cobra.Command{
		Use:          "binary [the absolute path to the binary]",
		Aliases:      []string{"file"},
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return scan.ValidateApplicationDependencies(applicationDeps)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
			defer cancel()
			config.FromTopDir, _ = cmd.Flags().GetString("topdir")
			config.FromInnerPath, _ = cmd.Flags().GetString("innerpath")
			config.UseRPMScan = false
			if config.FromTopDir == "" || config.FromInnerPath == "" {
				return errors.New("Both -t, --topdir or -i, --innerpath option are required")
			}
			res := validations.ScanBinary(ctx, config.FromTopDir, config.FromInnerPath, config.RPMIgnores, config.ErrIgnores)
			klog.V(1).InfoS("scanning result", res)
			if res.Skip {
				return nil
			}
			if !res.IsSuccess() && res.RPM != "" && config.IgnoreFileByRpm(config.FromInnerPath, res.RPM) {
				return nil
			}
			if res.IsSuccess() {
				klog.V(1).InfoS("scanning success", "topDir", config.FromTopDir, "innerPath", config.FromInnerPath, "status", "success")
			} else {
				status := res.Status()
				klog.InfoS("scanning "+status,
					"path", config.FromInnerPath,
					"status", status)
			}
			newResults := types.NewScanResults()
			newResults.Append(res)
			fmt.Printf("res: %v\n", res)
			fmt.Printf("newResults: %v\n", newResults)
			results[0] = newResults
			return nil
		},
	}
	gosyms.Flags().StringP("topdir", "t", "", "top dir of the path to the binary")
	gosyms.Flags().StringP("innerpath", "i", "", "innerpath of the path to the binary")

	scanCmd.AddCommand(scanPayload)
	scanCmd.AddCommand(scanJavaImage)
	scanCmd.AddCommand(scanNode)
	scanCmd.AddCommand(scanImage)
	scanCmd.AddCommand(gosyms)

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(scanCmd)

	// Add klog flags.
	klogFlags := flag.NewFlagSet("", flag.ExitOnError)
	klog.InitFlags(klogFlags)
	rootCmd.PersistentFlags().AddGoFlagSet(klogFlags)

	if err := rootCmd.Execute(); err != nil {
		klog.Fatalf("Error: %v\n", err)
	}
}

func getConfig(config *types.ConfigFile) error {
	// Handle --config.
	file := configFile
	if file == "" {
		file = defaultConfigFile
	}
	res, err := toml.DecodeFile(file, &config)
	if err == nil {
		klog.Infof("using config file: %v", file)
		if un := res.Undecoded(); len(un) != 0 {
			return fmt.Errorf("unknown keys in config: %+v", un)
		}
	} else if errors.Is(err, os.ErrNotExist) && configFile == "" {
		// When --config not specified and defaultConfigFile is not found,
		// fall back to embedded config.
		klog.Info("using embedded config")
		res, err = toml.Decode(embeddedConfig, &config)
		if err != nil { // Should never happen.
			panic("invalid embedded config: " + err.Error())
		}
		if un := res.Undecoded(); len(un) != 0 {
			panic(fmt.Errorf("unknown keys in config: %+v", un))
		}
	} else {
		// Otherwise, error out.
		return fmt.Errorf("can't parse config file %q: %w", file, err)
	}

	if configForVersion != "" {
		// Append to the main config.
		cfg, err := releases.GetConfigFor(configForVersion)
		if err != nil {
			return err
		}
		klog.Infof("adding rules from embedded config for %s", configForVersion)
		addConfig := &types.ConfigFile{}
		res, err = toml.Decode(string(cfg), &addConfig)
		if err != nil { // Should never happen.
			panic("invalid embedded config: " + err.Error())
		}
		if un := res.Undecoded(); len(un) != 0 {
			panic(fmt.Errorf("unknown keys in config: %+v", un))
		}
		if warn := config.Add(addConfig); warn != nil {
			klog.Warning(warn)
		}
	}

	return nil
}
