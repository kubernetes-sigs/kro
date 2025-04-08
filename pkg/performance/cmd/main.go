package cmd

import (
        "fmt"
        "os"
        "strings"
        "time"

        "github.com/spf13/cobra"
        
        "github.com/kro-run/kro/pkg/performance/analysis"
)

// NewRootCmd creates the root command
func NewRootCmd() *cobra.Command {
        // Root command
        rootCmd := &cobra.Command{
                Use:   "kro-perf",
                Short: "KRO Performance Testing Framework",
                Long:  `A comprehensive framework for benchmarking KRO performance.`,
        }

        // Add commands
        rootCmd.AddCommand(newBenchmarkCmd())
        rootCmd.AddCommand(newAnalyzeCmd())
        rootCmd.AddCommand(newVisualizeCmd())

        return rootCmd
}

// newBenchmarkCmd creates the benchmark command
func newBenchmarkCmd() *cobra.Command {
        var (
                benchmarkType    string
                duration         string
                workers          int
                resources        int
                namespace        string
                kubeconfig       string
                simulation       bool
                celComplexity    string
                graphComplexity  string
                nodes            int
                edges            int
                outputDir        string
                verbose          bool
        )

        cmd := &cobra.Command{
                Use:   "benchmark",
                Short: "Run KRO performance benchmarks",
                Long:  `Run performance benchmarks for KRO operations.`,
                Run: func(cmd *cobra.Command, args []string) {
                        fmt.Println("Running benchmarks...")
                        fmt.Printf("Type: %s, Duration: %s, Workers: %d, Resources: %d\n", 
                                benchmarkType, duration, workers, resources)
                        fmt.Printf("Simulation mode: %v\n", simulation)
                        
                        // Parse duration
                        parsedDuration, err := time.ParseDuration(duration)
                        if err != nil {
                                fmt.Printf("Error parsing duration: %v\n", err)
                                os.Exit(1)
                        }
                        
                        // Create benchmark config
                        config := &BenchmarkConfig{
                                Duration:        parsedDuration,
                                Workers:         workers,
                                ResourceCount:   resources,
                                Namespace:       namespace,
                                KubeConfig:      kubeconfig,
                                SimulationMode:  simulation,
                                CELComplexity:   celComplexity,
                                GraphComplexity: graphComplexity,
                                GraphNodes:      nodes,
                                GraphEdges:      edges,
                                OutputDirectory: outputDir,
                                VerboseOutput:   verbose,
                                BenchmarkType:   benchmarkType,
                        }
                        
                        // Create benchmark manager
                        manager := NewBenchmarkManager(config)
                        
                        // Run benchmarks
                        if err := manager.RunBenchmarks(); err != nil {
                                fmt.Printf("Error running benchmarks: %v\n", err)
                                os.Exit(1)
                        }
                        
                        fmt.Println("Benchmark run completed successfully.")
                },
        }

        // Add flags
        cmd.Flags().StringVar(&benchmarkType, "type", "all", "Benchmark type: crud, cel, resourcegraph, all")
        cmd.Flags().StringVar(&duration, "duration", "60s", "Duration of each benchmark")
        cmd.Flags().IntVar(&workers, "workers", 4, "Number of concurrent workers")
        cmd.Flags().IntVar(&resources, "resources", 100, "Number of resources for CRUD tests")
        cmd.Flags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace")
        cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
        cmd.Flags().BoolVar(&simulation, "simulation", true, "Run in simulation mode")
        cmd.Flags().StringVar(&celComplexity, "complexity", "all", "CEL expression complexity: simple, medium, complex, very-complex, all")
        cmd.Flags().StringVar(&graphComplexity, "graph-complexity", "all", "Graph complexity: small, medium, large, all")
        cmd.Flags().IntVar(&nodes, "nodes", 30, "Number of nodes for large graph tests")
        cmd.Flags().IntVar(&edges, "edges", 45, "Number of edges for large graph tests")
        cmd.Flags().StringVar(&outputDir, "output", "./results", "Output directory for results")
        cmd.Flags().BoolVar(&verbose, "verbose", false, "Enable verbose output")

        return cmd
}

// newAnalyzeCmd creates the analyze command
func newAnalyzeCmd() *cobra.Command {
        var (
                inputFile     string
                outputFile    string
                baselineFile  string
                format        string
                compareToLast bool
        )

        cmd := &cobra.Command{
                Use:   "analyze",
                Short: "Analyze benchmark results",
                Long:  `Analyze benchmark results and generate reports.`,
                Run: func(cmd *cobra.Command, args []string) {
                        fmt.Println("Analyzing benchmark results...")
                        fmt.Printf("Input: %s, Output: %s\n", inputFile, outputFile)
                        if baselineFile != "" {
                                fmt.Printf("Comparing to baseline: %s\n", baselineFile)
                        }
                        
                        // Create analyzer
                        analyzer := analysis.NewAnalyzer(inputFile, outputFile, baselineFile)
                        
                        // Run analysis
                        _, err := analyzer.Analyze()
                        if err != nil {
                                fmt.Printf("Error analyzing results: %v\n", err)
                                os.Exit(1)
                        }
                        
                        fmt.Println("Analysis completed.")
                },
        }

        // Add flags
        cmd.Flags().StringVar(&inputFile, "input", "", "Input results file to analyze")
        cmd.Flags().StringVar(&outputFile, "output", "", "Output file for analysis")
        cmd.Flags().StringVar(&baselineFile, "baseline", "", "Baseline results for comparison")
        cmd.Flags().StringVar(&format, "format", "json", "Output format: json, yaml, text")
        cmd.Flags().BoolVar(&compareToLast, "compare-to-last", false, "Compare to last run")

        // Mark required flags
        cmd.MarkFlagRequired("input")
        cmd.MarkFlagRequired("output")

        return cmd
}

// newVisualizeCmd creates the visualize command
func newVisualizeCmd() *cobra.Command {
        var (
                inputFile  string
                outputDir  string
                chartTypes string
        )

        cmd := &cobra.Command{
                Use:   "visualize",
                Short: "Visualize benchmark results",
                Long:  `Generate visualizations from benchmark results.`,
                Run: func(cmd *cobra.Command, args []string) {
                        fmt.Println("Generating visualizations...")
                        fmt.Printf("Input: %s, Output directory: %s\n", inputFile, outputDir)
                        fmt.Printf("Chart types: %s\n", chartTypes)
                        
                        // Parse chart types
                        chartTypesList := []string{"all"}
                        if chartTypes != "all" {
                                chartTypesList = strings.Split(chartTypes, ",")
                        }
                        
                        // Create visualizer
                        visualizer := analysis.NewVisualizer(inputFile, outputDir, chartTypesList)
                        
                        // Generate visualizations
                        if err := visualizer.Visualize(); err != nil {
                                fmt.Printf("Error generating visualizations: %v\n", err)
                                os.Exit(1)
                        }
                        
                        fmt.Println("Visualizations generated successfully.")
                },
        }

        // Add flags
        cmd.Flags().StringVar(&inputFile, "input", "", "Input analysis file to visualize")
        cmd.Flags().StringVar(&outputDir, "output", "", "Output directory for visualizations")
        cmd.Flags().StringVar(&chartTypes, "charts", "all", "Chart types to generate: bar, line, heatmap, all")

        // Mark required flags
        cmd.MarkFlagRequired("input")
        cmd.MarkFlagRequired("output")

        return cmd
}
