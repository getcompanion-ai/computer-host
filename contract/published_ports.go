package host

import "time"

type PublishedPortID string

type PublishedPort struct {
	ID        PublishedPortID `json:"id"`
	MachineID MachineID       `json:"machine_id"`
	Name      string          `json:"name,omitempty"`
	Port      uint16          `json:"port"`
	HostPort  uint16          `json:"host_port"`
	Protocol  PortProtocol    `json:"protocol"`
	CreatedAt time.Time       `json:"created_at"`
}

type CreatePublishedPortRequest struct {
	PublishedPortID PublishedPortID `json:"published_port_id"`
	Name            string          `json:"name,omitempty"`
	Port            uint16          `json:"port"`
	Protocol        PortProtocol    `json:"protocol"`
}

type CreatePublishedPortResponse struct {
	Port PublishedPort `json:"port"`
}

type ListPublishedPortsResponse struct {
	Ports []PublishedPort `json:"ports"`
}
