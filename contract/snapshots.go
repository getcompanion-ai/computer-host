package host

import "time"

type SnapshotID string

type Snapshot struct {
	ID        SnapshotID `json:"id"`
	MachineID MachineID  `json:"machine_id"`
	CreatedAt time.Time  `json:"created_at"`
}

type CreateSnapshotResponse struct {
	Snapshot Snapshot `json:"snapshot"`
}

type GetSnapshotResponse struct {
	Snapshot Snapshot `json:"snapshot"`
}

type ListSnapshotsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

type RestoreSnapshotRequest struct {
	MachineID MachineID `json:"machine_id"`
}

type RestoreSnapshotResponse struct {
	Machine Machine `json:"machine"`
}
