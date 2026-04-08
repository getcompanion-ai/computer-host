package firecracker

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultChrootRootDirName     = "root"
	defaultFirecrackerSocketDir  = "run"
	defaultFirecrackerSocketName = "firecracker.socket"
	defaultFirecrackerSocketPath = "/run/firecracker.socket"
)

type machinePaths struct {
	BaseDir       string
	ChrootRootDir string
	JailerBaseDir string
	SocketPath    string
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
	chrootRootDir := filepath.Join(jailerBaseDir, binName, string(id), defaultChrootRootDirName)

	return machinePaths{
		BaseDir:       baseDir,
		ChrootRootDir: chrootRootDir,
		JailerBaseDir: jailerBaseDir,
		SocketPath:    filepath.Join(chrootRootDir, defaultFirecrackerSocketDir, defaultFirecrackerSocketName),
	}, nil
}
