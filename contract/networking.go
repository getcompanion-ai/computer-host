package host

type MachinePortName string

type PortProtocol string

const (
	MachinePortNameSSH MachinePortName = "ssh"
	MachinePortNameVNC MachinePortName = "vnc"
)

const (
	PortProtocolTCP PortProtocol = "tcp"
)

type MachinePort struct {
	Name     MachinePortName `json:"name"`
	Port     uint16          `json:"port"`
	Protocol PortProtocol    `json:"protocol"`
}
