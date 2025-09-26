package api

import "fmt"

type Response[T any] struct {
	Code    int    `json:"code"`
	Data    T      `json:"data"`
	Message string `json:"message"`
	TraceID string `json:"x-traceID"`
}

func (e *Response[T]) Error() string {
	return fmt.Sprintf("%s (%d %s)", e.TraceID, e.Code, e.Message)
}

type GetAccessToken struct {
	ClientId     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type GetAccessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiredAt   string `json:"expiredAt"`
}

type GetFileList struct {
	ParentFileId   int64  `json:"parentFileId"`   // 文件夹ID，根目录传 0
	Page           int    `json:"page"`           // 页码数
	Limit          int    `json:"limit"`          // 每页文件数量，最大不超过100
	OrderBy        string `json:"orderBy"`        // 排序字段, 例如: file_id、size、file_name
	OrderDirection string `json:"orderDirection"` // 排序方向: asc、desc
	Trashed        bool   `json:"trashed"`        // 是否查看回收站的文件
}

type GetFileListResponse struct {
	FileList []File `json:"fileList"`
}

type GetFileInfoMulti struct {
	Fields []int64 `json:"fileIds"` // 文件ID数组
}

type GetFileInfoMultiResponse struct {
	FileList []FileInfo `json:"fileList"`
}

type File struct {
	FileId       int64  `json:"fileID"`       // 文件ID
	FileName     string `json:"fileName"`     // 文件名
	Type         int    `json:"type"`         // 0-文件 1-文件夹
	Size         int64  `json:"size"`         // 大小
	MD5          string `json:"etag"`         // MD5 (ETag
	Status       int    `json:"status"`       // 文件审核状态。 大于 100 为审核驳回文件
	ParentFileId int64  `json:"parentFileId"` // 目录ID
	ParentName   string `json:"parentName"`   // 目录名
	Category     int    `json:"category"`     // 0-未知 1-音频 2-视频 3-图片
	ContentType  string `json:"contentType"`  // 文件类型
}

type FileInfo struct {
	FileId       int64  `json:"fileId"`       // 文件ID
	FileName     string `json:"filename"`     // 文件名
	ParentFileId int64  `json:"parentFileId"` // 目录ID
	Type         int    `json:"type"`         // 0-file 1-folder
	MD5          string `json:"etag"`         // MD5 (ETag)
	Size         int64  `json:"size"`         // 大小
	Category     int    `json:"category"`     // 0-未知 1-音频 2-视频 3-图片
	Status       int    `json:"status"`       // 文件审核状态。 大于 100 为审核驳回文件
	PunishFlag   int    `json:"punishFlag"`   // 惩罚标记
	S3KeyFlag    string `json:"s3KeyFlag"`    // 关联 s3_key 的初始用户标识
	StorageNode  string `json:"storageNode"`  // m0 是 ceph，m1 以上为 minio
	Trashed      int    `json:"trashed"`      // 是否在回收站
	CreateAt     string `json:"createAt"`     // 创建时间
	UpdateAt     string `json:"updateAt"`     // 更新时间
}

type CompleteFile struct {
	File File
	Meta FileInfo
}

const (
	TypeFile   = 0
	TypeFolder = 1
)
