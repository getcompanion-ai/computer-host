package firecracker

import (
	"fmt"
	"path/filepath"
	"strings"
)

const defaultSocketName = "api.socket"

type machinePaths struct {
	BaseDir        string
	JailerBaseDir  string
	ChrootRootDir  string
	SocketName     string
	SocketPath     string
	FirecrackerBin string
}

func buildMachinePaths(rootDir string, id MachineID, firecrackerBinaryPath string) (machinePaths, error) {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return machinePaths{}, fmt.Errorf("root dir is required")
	}
	if strings.TrimSpace(string(id)) == "" {
		return machinePaths{}, fmt.Errorf("machine id is required")
	}
	binName := filepath.Base(strings.TrimSpace(firecrackerBinaryPath))
	if binName == "." || binName == string(filepath.Separator) || binName == "" {
		return machinePaths{}, fmt.Errorf("firecracker binary path is required")
	}

	baseDir := filepath.Join(rootDir, "machines", string(id))
	jailerBaseDir := filepath.Join(baseDir, "jailer")
	chrootRootDir := filepath.Join(jailerBaseDir, binName, string(id), "root")

	return machinePaths{
		BaseDir:        baseDir,
		JailerBaseDir:  jailerBaseDir,
		ChrootRootDir:  chrootRootDir,
		SocketName:     defaultSocketName,
		SocketPath:     filepath.Join(chrootRootDir, defaultSocketName),
		FirecrackerBin: binName,
	}, nil
}
