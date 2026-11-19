package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	goBinary := flag.String("go", "go", "Go executable")
	flag.Parse()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "cannot resolve contract test source")
		os.Exit(1)
	}
	testdataDir := filepath.Dir(sourceFile)
	repoRoot := filepath.Clean(filepath.Join(testdataDir, "..", "..", ".."))
	mihomoRoot := os.Getenv("MIHOMO_SOURCE")
	if mihomoRoot == "" {
		mihomoRoot = filepath.Clean(filepath.Join(
			repoRoot,
			"..",
			"upstream",
			"mihomo-integrated-verified-3f27ef39f8c96bbfbc8b0b9bb10f5c9ef60149d8",
		))
	}
	mihomoRoot, err := filepath.Abs(mihomoRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	virtualTest := filepath.Join(mihomoRoot, "adapter", "parser_ghostlink_test.go")
	overlayData, err := json.Marshal(map[string]any{
		"Replace": map[string]string{
			virtualTest: filepath.Join(testdataDir, "mihomo_contract_round9_test.go"),
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	overlay, err := os.CreateTemp("", "ghostlink-mihomo-overlay-*.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	overlayPath := overlay.Name()
	defer os.Remove(overlayPath)
	if _, err := overlay.Write(overlayData); err != nil {
		_ = overlay.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := overlay.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cmd := exec.Command(
		*goBinary,
		"test",
		"-tags=http2legacy",
		"-overlay",
		overlayPath,
		"./adapter",
		"-run",
		"^TestGhostLinkRound9YAMLContract$",
		"-count=1",
	)
	cmd.Dir = mihomoRoot
	cmd.Env = append(
		os.Environ(),
		"GHOSTLINK_CONTRACT_YAML="+filepath.Join(
			testdataDir,
			"ghostlink-mihomo-contract.yaml",
		),
		"GODEBUG=http2xconnect=1",
		"GOTOOLCHAIN=local",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
