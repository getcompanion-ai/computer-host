package firecracker

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultChrootRootDirName     = "root"
	defaultLogDirName            = "logs"
	defaultSerialLogName         = "serial.log"
	defaultFirecrackerSocketDir  = "run"
	defaultFirecrackerLogName    = "firecracker.log"
	defaultFirecrackerSocketName = "firecracker.socket"
	defaultFirecrackerSocketPath = "/run/firecracker.socket"
)

type machinePaths struct {
	BaseDir                  string
	ChrootRootDir            string
	JailerBaseDir            string
	LogDir                   string
	FirecrackerLogPath       string
	JailedFirecrackerLogPath string
	SerialLogPath            string
	JailedSerialLogPath      string
	PIDFilePath              string
	SocketPath               string
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
	logDir := filepath.Join(chrootRootDir, defaultLogDirName)

	return machinePaths{
		BaseDir:                  baseDir,
		ChrootRootDir:            chrootRootDir,
		JailerBaseDir:            jailerBaseDir,
		LogDir:                   logDir,
		FirecrackerLogPath:       filepath.Join(logDir, defaultFirecrackerLogName),
		JailedFirecrackerLogPath: path.Join("/", defaultLogDirName, defaultFirecrackerLogName),
		SerialLogPath:            filepath.Join(logDir, defaultSerialLogName),
		JailedSerialLogPath:      path.Join("/", defaultLogDirName, defaultSerialLogName),
		PIDFilePath:              filepath.Join(chrootRootDir, binName+".pid"),
		SocketPath:               filepath.Join(chrootRootDir, defaultFirecrackerSocketDir, defaultFirecrackerSocketName),
	}, nil
}

func procSocketPath(pid int) string {
	return filepath.Join("/proc", strconv.Itoa(pid), "root", defaultFirecrackerSocketDir, defaultFirecrackerSocketName)
}
