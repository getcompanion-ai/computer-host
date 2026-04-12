package host

type MachinePortName string

type PortProtocol string

const (
	MachinePortNameSSH  MachinePortName = "ssh"
	MachinePortNameVNC  MachinePortName = "vnc"
	MachinePortNameExec MachinePortName = "exec"
)

const (
	PortProtocolTCP PortProtocol = "tcp"
)

type MachinePort struct {
	Name     MachinePortName `json:"name"`
	Port     uint16          `json:"port"`
	HostPort uint16          `json:"host_port,omitempty"`
	Protocol PortProtocol    `json:"protocol"`
}
