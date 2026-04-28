package host

type FileOperation string

const (
	FileOperationReadText   FileOperation = "read_text"
	FileOperationReadBytes  FileOperation = "read_bytes"
	FileOperationWriteText  FileOperation = "write_text"
	FileOperationWriteBytes FileOperation = "write_bytes"
	FileOperationRemove     FileOperation = "remove"
	FileOperationList       FileOperation = "list"
	FileOperationStat       FileOperation = "stat"
	FileOperationExists     FileOperation = "exists"
	FileOperationPatch      FileOperation = "patch"
	FileOperationReadRange  FileOperation = "read_range"
	FileOperationWriteRange FileOperation = "write_range"
	FileOperationMkdir      FileOperation = "mkdir"
	FileOperationMove       FileOperation = "move"
	FileOperationGrep       FileOperation = "grep"
)

type FileEntryType string

const (
	FileEntryTypeFile      FileEntryType = "file"
	FileEntryTypeDirectory FileEntryType = "directory"
)

type FileEntry struct {
	Path      string        `json:"path"`
	Name      string        `json:"name"`
	Type      FileEntryType `json:"type"`
	SizeBytes int64         `json:"size_bytes,omitempty"`
	Mode      int64         `json:"mode,omitempty"`
}

type FileStat struct {
	Path      string        `json:"path"`
	Type      FileEntryType `json:"type"`
	SizeBytes int64         `json:"size_bytes,omitempty"`
	Mode      int64         `json:"mode,omitempty"`
}

type FileReadTextRequest struct {
	Path string `json:"path"`
}

type FileReadTextResponse struct {
	Content string `json:"content"`
}

type FileReadBytesRequest struct {
	Path string `json:"path"`
}

type FileReadBytesResponse struct {
	ContentBase64 string `json:"content_base64"`
}

type FileWriteTextRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    *int64 `json:"mode,omitempty"`
}

type FileWriteBytesRequest struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Mode          *int64 `json:"mode,omitempty"`
}

type FileRemoveRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FileListRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FileListResponse struct {
	Entries []FileEntry `json:"entries"`
}

type FileStatRequest struct {
	Path string `json:"path"`
}

type FileStatResponse struct {
	Stat FileStat `json:"stat"`
}

type FileExistsRequest struct {
	Path string `json:"path"`
}

type FileExistsResponse struct {
	Exists bool `json:"exists"`
}

type FilePatchEdit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

type FilePatchRequest struct {
	Path        string          `json:"path"`
	Edits       []FilePatchEdit `json:"edits,omitempty"`
	SetContents *string         `json:"set_contents,omitempty"`
}

type FilePatchResponse struct {
	Version int64 `json:"version"`
}

type FileReadRangeRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

type FileReadRangeResponse struct {
	ContentBase64 string `json:"content_base64"`
}

type FileWriteRangeRequest struct {
	Path          string `json:"path"`
	Offset        int64  `json:"offset"`
	ContentBase64 string `json:"content_base64"`
}

type FileMkdirRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FileMoveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type FileGrepRequest struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Regex           bool   `json:"regex,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	MaxMatches      int    `json:"max_matches,omitempty"`
}

type FileGrepMatch struct {
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Match string `json:"match"`
}

type FileGrepResponse struct {
	Matches []FileGrepMatch `json:"matches"`
}

type FileOperationRequest struct {
	Operation       FileOperation   `json:"operation"`
	Path            string          `json:"path,omitempty"`
	To              string          `json:"to,omitempty"`
	Content         string          `json:"content,omitempty"`
	ContentBase64   string          `json:"content_base64,omitempty"`
	Mode            *int64          `json:"mode,omitempty"`
	Recursive       bool            `json:"recursive,omitempty"`
	Offset          int64           `json:"offset,omitempty"`
	Length          int64           `json:"length,omitempty"`
	Edits           []FilePatchEdit `json:"edits,omitempty"`
	SetContents     *string         `json:"set_contents,omitempty"`
	Pattern         string          `json:"pattern,omitempty"`
	Regex           bool            `json:"regex,omitempty"`
	CaseInsensitive bool            `json:"case_insensitive,omitempty"`
	MaxMatches      int             `json:"max_matches,omitempty"`
}

type FileOperationResponse struct {
	Content       string          `json:"content,omitempty"`
	ContentBase64 string          `json:"content_base64,omitempty"`
	Entries       []FileEntry     `json:"entries,omitempty"`
	Stat          *FileStat       `json:"stat,omitempty"`
	Exists        *bool           `json:"exists,omitempty"`
	Version       int64           `json:"version,omitempty"`
	Matches       []FileGrepMatch `json:"matches,omitempty"`
}
