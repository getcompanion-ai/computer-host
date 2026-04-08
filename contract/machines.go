package host

type CreateMachineRequest struct {
	MachineID MachineID `json:"machine_id"`
}

type CreateMachineResponse struct {
	Machine Machine `json:"machine"`
}

type GetMachineResponse struct {
	Machine Machine `json:"machine"`
}

type ListMachinesResponse struct {
	Machines []Machine `json:"machines"`
}
