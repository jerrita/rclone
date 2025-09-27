package api

import (
	"fmt"
	"time"
)

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

type GetFileDetail struct {
	FileId int64 `json:"fileID"`
}

type GetFileDetailResponse struct {
	FileId       int64  `json:"fileID"`       // 文件ID
	FileName     string `json:"filename"`     // 文件名
	Type         int    `json:"type"`         // 0-file 1-folder
	Size         int64  `json:"size"`         // 大小
	MD5          string `json:"etag"`         // MD5 (ETag)
	Status       int    `json:"status"`       // 文件审核状态。 大于 100 为审核驳回文件
	ParentFileId int64  `json:"parentFileId"` // 目录ID
	CreateAt     string `json:"createAt"`     // 创建时间
	Trashed      int    `json:"trashed"`      // 是否在回收站
}

type GetFileInfoMulti struct {
	Fields []int64 `json:"fileIds"` // 文件ID数组
}

type GetFileInfoMultiResponse struct {
	FileList []FileInfo `json:"fileList"`
}

type GetDownloadInfo struct {
	FileId int64 `json:"fileId"`
}

type GetDownloadInfoResponse struct {
	DownloadUrl string `json:"downloadUrl"`
}

type CreateFileRequest struct {
	ParentFileID int64  `json:"parentFileID"` // 父目录ID
	Filename     string `json:"filename"`     // 文件名
	Etag         string `json:"etag"`         // 文件MD5
	Size         int64  `json:"size"`         // 文件大小
	Duplicate    int    `json:"duplicate"`    // 文件处理策略（1保留两者，2覆盖原文件）
	ContainDir   bool   `json:"containDir"`   // 上传文件是否包含路径
}

type CreateFileResponse struct {
	FileID      int64    `json:"fileID"`      // 文件ID（秒传时返回）
	PreUploadId string   `json:"preuploadID"` // 预上传ID
	Reuse       bool     `json:"reuse"`       // 是否秒传
	SliceSize   int64    `json:"sliceSize"`   // 分片大小
	Servers     []string `json:"servers"`     // 上传地址列表
}

type UploadSliceRequest struct {
	PreUploadId string `json:"preuploadID"` // 预上传ID
	SliceNo     int    `json:"sliceNo"`     // 分片序号，从1开始
	SliceMD5    string `json:"sliceMD5"`    // 当前分片MD5
	// Slice field is handled as multipart file upload, not JSON
}

type UploadCompleteRequest struct {
	PreUploadId string `json:"preuploadID"` // 预上传ID
}

type UploadCompleteResponse struct {
	Completed bool  `json:"completed"` // 上传是否完成
	FileID    int64 `json:"fileID"`    // 上传完成文件ID
}

type MkdirRequest struct {
	Name     string `json:"name"`     // 父目录ID
	ParentId string `json:"parentID"` // 目录名
}

type MkdirResponse struct {
	DirId int64 `json:"dirID"` // 目录ID
}

type FileTrashRequest struct {
	FileIDs []int64 `json:"fileIDs"` // 要删除的文件ID数组，一次性最大不能超过100个文件
}

type FileTrashResponse struct {
	// Usually empty on success, data is null
}

type GetUploadDomainsResponse struct {
	Domains []string `json:"data"` // 上传域名列表
}

type SingleUploadRequest struct {
	ParentFileID int64  `json:"parentFileID"` // 父目录ID
	Filename     string `json:"filename"`     // 文件名
	Etag         string `json:"etag"`         // 文件MD5
	Size         int64  `json:"size"`         // 文件大小
	Duplicate    int    `json:"duplicate"`    // 文件处理策略（1保留两者，2覆盖原文件）
	ContainDir   bool   `json:"containDir"`   // 上传文件是否包含路径
	// File field is handled as multipart file upload, not JSON
}

type SingleUploadResponse struct {
	FileID    int64 `json:"fileID"`    // 文件ID
	Completed bool  `json:"completed"` // 是否上传完成
}

type FileMoveRequest struct {
	FileIds      []int64 `json:"fileIdList"`   // 要移动的文件ID列表
	ParentFileId int64   `json:"parentFileId"` // 目标父目录ID
}

type NullResponse struct {
	// Usually empty on success
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
	FileId       int64     // 文件ID
	FileName     string    // 文件名
	ParentFileId int64     // 目录ID
	Type         int       // 0-file 1-folder
	MD5          string    // MD5 (ETag)
	Size         int64     // 大小
	Status       int       // 文件审核状态。 大于 100 为审核驳回文件
	Trashed      int       // 是否在回收站
	ModTime      time.Time // 更新时间
}

const (
	TypeFile   = 0
	TypeFolder = 1
)
