package pan123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/pan123/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/time/rate"
)

type Api struct {
	uri     string
	method  string
	limiter *rate.Limiter
}

const (
	rootUrl        = "https://open-api.123pan.com"
	timeMetaLayout = "2006-01-02 15:04:05"
)

// Globals
var (
	preRefreshDuration = 7 * 24 * time.Hour

	apiAccessToken      = Api{"/api/v1/access_token", "POST", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileTrash        = Api{"/api/v1/file/trash", "POST", rate.NewLimiter(rate.Limit(5), 5)}
	apiFileList         = Api{"/api/v1/file/list", "GET", rate.NewLimiter(rate.Limit(4), 4)}
	apiFileInfoMulti    = Api{"/api/v1/file/infos", "POST", rate.NewLimiter(rate.Limit(4), 4)}
	apiFileDownload     = Api{"/api/v1/file/download_info", "GET", rate.NewLimiter(rate.Limit(5), 5)}
	apiMkdir            = Api{"/upload/v1/file/mkdir", "POST", rate.NewLimiter(rate.Limit(2), 2)}
	apiFileCreate       = Api{"/upload/v2/file/create", "POST", rate.NewLimiter(rate.Limit(5), 5)}
	apiUploadSlice      = Api{"/upload/v2/file/slice", "POST", rate.NewLimiter(rate.Limit(20), 20)}
	apiUploadComplete   = Api{"/upload/v2/file/upload_complete", "POST", rate.NewLimiter(rate.Limit(20), 20)}
	apiGetUploadDomains = Api{"/upload/v2/file/domain", "GET", rate.NewLimiter(rate.Limit(5), 5)}
	apiSingleUpload     = Api{"/upload/v2/file/single/create", "POST", rate.NewLimiter(rate.Limit(5), 5)}
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pan123",
		Description: "123 Yun Pan",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "client_id",
			Help:      "pan123 open client_id",
			Required:  true,
			Sensitive: false,
		}, {
			Name:      "client_secret",
			Help:      "pan123 open client_secret",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "root_id",
			Help:     "pan123 root folder id",
			Default:  0,
			Required: true,
		}, {
			Name:     "access_token",
			Help:     "pan123 open access_token",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "expired_at",
			Help:     "pan123 open expired_at",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "get_mod_time_when_list",
			Help:     "when set, rclone will fetch modTime when list files, which will cause extra transitions",
			Default:  true,
			Advanced: true,
		}},
	})
}

func (f *Fs) authorizeAccount(ctx context.Context) error {
	if f.opt.AccessToken != "" && f.opt.ExpiredAt != "" {
		parsedTime, err := time.Parse(time.RFC3339, f.opt.ExpiredAt)
		if err != nil {
			return err
		}
		if time.Now().Add(preRefreshDuration).Before(parsedTime) {
			f.srv.SetHeader("Authorization", "Bearer "+f.opt.AccessToken)
			return nil
		}
	}

	opts := rest.Opts{
		Method: apiAccessToken.method,
		Path:   apiAccessToken.uri,
	}
	request := api.GetAccessToken{
		ClientId:     f.opt.ClientId,
		ClientSecret: f.opt.ClientSecret,
	}

	_ = apiAccessToken.limiter.Wait(ctx)
	resp := api.Response[api.GetAccessTokenResponse]{}
	_, err := f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 0 {
		return fmt.Errorf("failed to get access token: %d (%s)", resp.Code, resp.Message)
	}

	f.srv.SetHeader("Authorization", "Bearer "+resp.Data.AccessToken)
	f.opt.AccessToken = resp.Data.AccessToken
	f.opt.ExpiredAt = resp.Data.ExpiredAt
	f.m.Set("access_token", resp.Data.AccessToken)
	f.m.Set("expired_at", resp.Data.ExpiredAt)
	return nil
}

type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	opt      Options            // parsed options
	features *fs.Features       // optional features
	srv      *rest.Client       // the connection to the server
	curl     *rest.Client       // for download
	dirCache *dircache.DirCache // Map of directory path to directory id
	m        configmap.Mapper   // configmap.Mapper

	// Upload domains cache
	uploadDomains       []string  // cached upload domains
	uploadDomainsExpiry time.Time // expiry time for upload domains cache
}

type Options struct {
	ClientId           string `config:"client_id"`
	ClientSecret       string `config:"client_secret"`
	RootId             int64  `config:"root_id"`
	AccessToken        string `config:"access_token"`
	ExpiredAt          string `config:"expired_at"`
	GetModTimeWhenList bool   `config:"get_mod_time_when_list"`
}

type Object struct {
	fs      *Fs       // what this object is part of
	remote  string    // The remote path
	size    int64     // size of the object
	modTime time.Time // modification time of the object
	id      string    // ID of the object
	md5     string    // MD5 of the object content
}

func (o *Object) Fs() fs.Info {
	return o.fs
}

func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

func (o *Object) Remote() string {
	return o.remote
}

func (o *Object) ModTime(ctx context.Context) time.Time {
	if o.modTime.IsZero() {
		// TODO: call single file detail api
		panic("modTime getter not implemented yet")
	}
	return o.modTime
}

func (o *Object) Size() int64 {
	return o.size
}

func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	if ty != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5, nil
}

func (o *Object) Storable() bool {
	return true
}

func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	o.modTime = t
	return nil
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.FixRangeOption(options, o.size)

	opts := rest.Opts{
		Method: apiFileDownload.method,
		Path:   apiFileDownload.uri,
	}
	request := api.GetDownloadInfo{FileId: toId(o.id)}
	resp := api.Response[api.GetDownloadInfoResponse]{}

	_ = apiFileDownload.limiter.Wait(ctx)
	_, err := o.fs.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("failed to download: %d (%s)", resp.Code, resp.Message)
	}

	fs.Debugf(o, "download url: %s (%s) => %s", o.remote, o.id, resp.Data.DownloadUrl)
	opts.RootURL = resp.Data.DownloadUrl
	opts.Path = ""
	opts.Method = "GET"
	d, err := o.fs.curl.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}
	return d.Body, err
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.fs.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}

	// Update the object with new metadata
	newO := newObj.(*Object)
	o.size = newO.size
	o.modTime = newO.modTime
	o.md5 = newO.md5
	o.id = newO.id

	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	opts := rest.Opts{
		Method: apiFileTrash.method,
		Path:   apiFileTrash.uri,
	}

	request := api.FileTrashRequest{
		FileIDs: []int64{toId(o.id)},
	}

	resp := api.Response[api.FileTrashResponse]{}

	_ = apiFileTrash.limiter.Wait(ctx)
	_, err := o.fs.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 0 {
		return fmt.Errorf("failed to delete file: %d (%s)", resp.Code, resp.Message)
	}

	return nil
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("pan123 root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision returns the mod time of this Fs
func (f *Fs) Precision() time.Duration {
	return time.Millisecond
}

// Hashes Returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	files, err := f.listAll(ctx, toId(pathID))
	if err != nil {
		fs.Debugf(f, "listAll: %v", err)
		return "", false, err
	}

	for _, file := range files {
		if file.Type == api.TypeFile {
			continue
		}
		if strings.EqualFold(file.FileName, leaf) {
			return toString(file.FileId), true, nil
		}
	}

	return pathIDOut, false, nil
}

func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	opts := rest.Opts{
		Method: apiMkdir.method,
		Path:   apiMkdir.uri,
	}

	request := api.MkdirRequest{
		Name:     leaf,
		ParentId: pathID,
	}

	resp := api.Response[api.MkdirResponse]{}

	_ = apiMkdir.limiter.Wait(ctx)
	_, err = f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return "", err
	}

	if resp.Code != 0 {
		return "", fmt.Errorf("failed to create directory: %d (%s)", resp.Code, resp.Message)
	}

	return toString(resp.Data.DirId), nil
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.Response[string])
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	return errResponse
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	if opt.ClientId == "" || opt.ClientSecret == "" {
		return nil, errors.New("client_id and client_secret are required")
	}

	root = strings.Trim(root, "/")
	client := fshttp.NewClient(ctx)
	srv := rest.NewClient(client).SetRoot(rootUrl)
	srv.SetHeader("Platform", "open_platform")
	srv.SetHeader("Content-Type", "application/json")

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		srv:  srv,
		curl: rest.NewClient(client),
		m:    m,
	}

	f.features = (&fs.Features{
		CaseInsensitive:         true,
		CanHaveEmptyDirectories: true,
		SlowModTime:             true,
	}).Fill(ctx, f)
	f.srv.SetErrorHandler(errorHandler)

	err = f.authorizeAccount(ctx)
	if err != nil {
		return nil, err
	}

	rootId := f.opt.RootId
	f.dirCache = dircache.New(root, toString(rootId), f)
	err = f.dirCache.FindRoot(ctx, true)
	if err != nil {
		// Assume it is a file
		newRoot, _ := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, toString(rootId), &tempF)
		tempF.root = newRoot
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}

		f.features.Fill(ctx, &tempF)
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as a receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root

		// return an error with a fs which points to the parent
		return f, fs.ErrorIsFile
	}

	return f, nil
}

func (f *Fs) listAll(ctx context.Context, dirId int64) ([]api.CompleteFile, error) {
	files := make([]api.CompleteFile, 0)
	fileMap := map[int64]api.CompleteFile{}
	page := 1

	optsFile := rest.Opts{
		Method: apiFileList.method,
		Path:   apiFileList.uri,
	}
	optsMeta := rest.Opts{
		Method: apiFileInfoMulti.method,
		Path:   apiFileInfoMulti.uri,
	}
	requestFile := api.GetFileList{
		ParentFileId:   dirId,
		Page:           page,
		Limit:          100,
		OrderBy:        "file_name",
		OrderDirection: "asc",
		Trashed:        false,
	}
	requestMeta := api.GetFileInfoMulti{}
	respFile := api.Response[api.GetFileListResponse]{}
	respMeta := api.Response[api.GetFileInfoMultiResponse]{}

	for {
		_ = apiFileList.limiter.Wait(ctx)
		_, err := f.srv.CallJSON(ctx, &optsFile, &requestFile, &respFile)
		if err != nil {
			return nil, err
		}
		if respFile.Code != 0 {
			return nil, fmt.Errorf("failed to list files: %d (%s)", respFile.Code, respFile.Message)
		}

		requestMeta.Fields = make([]int64, 0)
		for _, file := range respFile.Data.FileList {
			cf := api.CompleteFile{
				FileId:       file.FileId,
				FileName:     file.FileName,
				ParentFileId: file.ParentFileId,
				Type:         file.Type,
				MD5:          file.MD5,
				Size:         file.Size,
				Status:       file.Status,
			}
			fileMap[file.FileId] = cf
			requestMeta.Fields = append(requestMeta.Fields, file.FileId)
		}

		if len(requestMeta.Fields) == 0 {
			break
		}

		if f.opt.GetModTimeWhenList {
			_ = apiFileInfoMulti.limiter.Wait(ctx)
			_, err = f.srv.CallJSON(ctx, &optsMeta, &requestMeta, &respMeta)
			if err != nil {
				return nil, err
			}
			if respMeta.Code != 0 {
				return nil, fmt.Errorf("failed to list file meta: %d (%s)", respMeta.Code, respMeta.Message)
			}

			for _, meta := range respMeta.Data.FileList {
				loc, _ := time.LoadLocation("Local")
				modTime, err := time.ParseInLocation(timeMetaLayout, meta.UpdateAt, loc)
				if err != nil {
					fs.Errorf(f, "cannot parse modified time: %v", err)
				}
				// Find the corresponding file in fileMap and add modTime
				if cf, exists := fileMap[meta.FileId]; exists {
					cf.ModTime = modTime
					files = append(files, cf)
				} else {
					cf := api.CompleteFile{
						FileId:       meta.FileId,
						FileName:     meta.FileName,
						ParentFileId: meta.ParentFileId,
						Type:         meta.Type,
						MD5:          meta.MD5,
						Size:         meta.Size,
						Status:       meta.Status,
						ModTime:      modTime,
					}
					files = append(files, cf)
				}
			}
		} else {
			for _, file := range fileMap {
				files = append(files, file)
			}
		}

		if len(respFile.Data.FileList) < 100 {
			break
		}
		page++
		requestFile.Page = page
	}

	return files, nil
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(nil, "list called with dir: %s", dir)
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}

	list, err := f.listAll(ctx, toId(directoryID))
	for _, file := range list {
		remote := path.Join(dir, file.FileName)
		if file.Type == api.TypeFolder {
			f.dirCache.Put(remote, toString(file.FileId))
			d := fs.NewDir(remote, file.ModTime).SetID(toString(file.FileId)).SetParentID(toString(f.opt.RootId))
			entries = append(entries, d)
		} else {
			o, err := f.NewObjectComplete(ctx, remote, file)
			if err == nil {
				entries = append(entries, o)
			} else {
				fs.Debugf(nil, "Failed to list object: %v", err)
			}
		}
	}

	return entries, err
}

func (f *Fs) NewObjectComplete(ctx context.Context, remote string, file api.CompleteFile) (fs.Object, error) {
	o := &Object{
		fs:      f,
		remote:  remote,
		size:    file.Size,
		modTime: file.ModTime,
		id:      toString(file.FileId),
		md5:     file.MD5,
	}

	return o, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, dirId, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	files, err := f.listAll(ctx, toId(dirId))
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if file.Type == api.TypeFile && file.FileName == leaf {
			o := &Object{
				fs:      f,
				remote:  remote,
				size:    file.Size,
				modTime: file.ModTime,
				id:      toString(file.FileId),
				md5:     file.MD5,
			}
			return o, nil
		}
	}

	return nil, fs.ErrorObjectNotFound
}

// Put uploads a new file
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()

	// Calculate file MD5
	var buf []byte
	var err error
	if size >= 0 {
		buf = make([]byte, size)
		_, err = io.ReadFull(in, buf)
		if err != nil {
			return nil, fmt.Errorf("failed to read file: %w", err)
		}
	} else {
		buf, err = io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("failed to read file: %w", err)
		}
		size = int64(len(buf))
	}

	// Calculate MD5
	hasher := md5.New()
	hasher.Write(buf)
	md5Hash := hex.EncodeToString(hasher.Sum(nil))

	// Get parent directory
	root, leaf := dircache.SplitPath(remote)
	parentID, err := f.dirCache.FindDir(ctx, root, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find/create parent directory: %w", err)
	}

	var fileID int64

	// Choose upload method based on file size
	// Use single upload for files < 1GB (1073741824 bytes)
	if size < 1073741824 {
		fs.Debugf(f, "Using single upload for file %s (size: %d bytes)", remote, size)
		fileID, err = f.singleUpload(ctx, toId(parentID), leaf, md5Hash, size, buf)
		if err != nil {
			return nil, fmt.Errorf("failed to single upload: %w", err)
		}
	} else {
		fs.Debugf(f, "Using chunked upload for file %s (size: %d bytes)", remote, size)
		// Create file
		createResp, err := f.createFile(ctx, toId(parentID), leaf, md5Hash, size)
		if err != nil {
			return nil, fmt.Errorf("failed to create file: %w", err)
		}

		// If it's a reuse (instant upload), return object
		if createResp.Reuse {
			cf := api.CompleteFile{
				FileId:       createResp.FileID,
				FileName:     leaf,
				ParentFileId: toId(parentID),
				MD5:          md5Hash,
				Size:         size,
			}
			return f.NewObjectComplete(ctx, remote, cf)
		}

		// Upload file in chunks
		err = f.uploadChunks(ctx, buf, createResp)
		if err != nil {
			return nil, fmt.Errorf("failed to upload chunks: %w", err)
		}

		// Complete upload
		fileID, err = f.completeUpload(ctx, createResp.PreUploadId)
		if err != nil {
			return nil, fmt.Errorf("failed to complete upload: %w", err)
		}
	}

	cf := api.CompleteFile{
		FileId:       fileID,
		FileName:     leaf,
		ParentFileId: toId(parentID),
		MD5:          md5Hash,
		Size:         size,
	}
	return f.NewObjectComplete(ctx, remote, cf)
}

// createFile creates a file entry and returns upload info
func (f *Fs) createFile(ctx context.Context, parentID int64, filename, etag string, size int64) (*api.CreateFileResponse, error) {
	opts := rest.Opts{
		Method: apiFileCreate.method,
		Path:   apiFileCreate.uri,
	}

	request := api.CreateFileRequest{
		ParentFileID: parentID,
		Filename:     filename,
		Etag:         etag,
		Size:         size,
		Duplicate:    2,     // Always overwrite existing files
		ContainDir:   false, // Don't use path-based uploads
	}

	resp := api.Response[api.CreateFileResponse]{}

	_ = apiFileCreate.limiter.Wait(ctx)
	_, err := f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("failed to create file: %d (%s)", resp.Code, resp.Message)
	}

	return &resp.Data, nil
}

// uploadChunks uploads file chunks to the server
func (f *Fs) uploadChunks(ctx context.Context, data []byte, createResp *api.CreateFileResponse) error {
	if len(createResp.Servers) == 0 {
		return errors.New("no upload servers available")
	}

	// Use the first server
	uploadServer := createResp.Servers[0]
	sliceSize := int(createResp.SliceSize)

	// Split file into chunks
	totalChunks := (len(data) + sliceSize - 1) / sliceSize

	for i := 0; i < totalChunks; i++ {
		start := i * sliceSize
		end := start + sliceSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[start:end]

		// Calculate chunk MD5
		hasher := md5.New()
		hasher.Write(chunk)
		chunkMD5 := hex.EncodeToString(hasher.Sum(nil))

		err := f.uploadChunk(ctx, uploadServer, createResp.PreUploadId, i+1, chunkMD5, chunk)
		if err != nil {
			return fmt.Errorf("failed to upload chunk %d: %w", i+1, err)
		}
	}

	return nil
}

// uploadChunk uploads a single chunk
func (f *Fs) uploadChunk(ctx context.Context, server, preuploadID string, sliceNo int, sliceMD5 string, chunk []byte) error {
	// Create multipart form data
	formData := url.Values{}
	formData.Set("preuploadID", preuploadID)
	formData.Set("sliceNo", strconv.Itoa(sliceNo))
	formData.Set("sliceMD5", sliceMD5)

	formReader, contentType, overhead, err := rest.MultipartUpload(ctx, bytes.NewReader(chunk), formData, "slice", "chunk")
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}

	contentLength := overhead + int64(len(chunk))

	opts := rest.Opts{
		Method:        apiUploadSlice.method,
		RootURL:       server,
		Path:          apiUploadSlice.uri,
		Body:          formReader,
		ContentType:   contentType,
		ContentLength: &contentLength,
	}

	resp := api.Response[interface{}]{}

	_ = apiUploadSlice.limiter.Wait(ctx)
	_, err = f.srv.CallJSON(ctx, &opts, nil, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 0 {
		return fmt.Errorf("failed to upload chunk: %d (%s)", resp.Code, resp.Message)
	}

	return nil
}

// completeUpload notifies the server that upload is complete and returns file ID
func (f *Fs) completeUpload(ctx context.Context, preuploadID string) (int64, error) {
	opts := rest.Opts{
		Method: apiUploadComplete.method,
		Path:   apiUploadComplete.uri,
	}

	request := api.UploadCompleteRequest{
		PreUploadId: preuploadID,
	}

	resp := api.Response[api.UploadCompleteResponse]{}

	// May need to retry/poll until upload is complete
	for {
		_ = apiUploadComplete.limiter.Wait(ctx)
		_, err := f.srv.CallJSON(ctx, &opts, &request, &resp)
		if err != nil {
			return 0, err
		}

		if resp.Code != 0 && resp.Code != 20103 {
			return 0, fmt.Errorf("failed to complete upload: %d (%s)", resp.Code, resp.Message)
		}

		if resp.Data.Completed {
			return resp.Data.FileID, nil
		}

		// Wait 1 second before retrying as per documentation
		time.Sleep(1 * time.Second)
	}
}

// getUploadDomains gets the upload domains for single file upload with caching
func (f *Fs) getUploadDomains(ctx context.Context) ([]string, error) {
	// Check if cached domains are still valid (cache for 1 hour)
	if len(f.uploadDomains) > 0 && time.Now().Before(f.uploadDomainsExpiry) {
		fs.Debugf(f, "Using cached upload domains (%d domains)", len(f.uploadDomains))
		return f.uploadDomains, nil
	}

	fs.Debugf(f, "Fetching upload domains from API")
	opts := rest.Opts{
		Method: apiGetUploadDomains.method,
		Path:   apiGetUploadDomains.uri,
	}

	resp := api.Response[api.GetUploadDomainsResponse]{}

	_ = apiGetUploadDomains.limiter.Wait(ctx)
	_, err := f.srv.CallJSON(ctx, &opts, nil, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("failed to get upload domains: %d (%s)", resp.Code, resp.Message)
	}

	// Cache the domains for 1 hour
	f.uploadDomains = resp.Data
	f.uploadDomainsExpiry = time.Now().Add(1 * time.Hour)

	fs.Debugf(f, "Cached %d upload domains, expires at %v", len(f.uploadDomains), f.uploadDomainsExpiry)
	return f.uploadDomains, nil
}

// clearUploadDomainsCache clears the cached upload domains
func (f *Fs) clearUploadDomainsCache() {
	f.uploadDomains = nil
	f.uploadDomainsExpiry = time.Time{}
	fs.Debugf(f, "Upload domains cache cleared")
}

// singleUpload uploads a file using single upload API for files < 1GB
func (f *Fs) singleUpload(ctx context.Context, parentID int64, filename, md5Hash string, size int64, data []byte) (int64, error) {
	// Get upload domains
	domains, err := f.getUploadDomains(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get upload domains: %w", err)
	}

	if len(domains) == 0 {
		// Clear cache and try once more if no domains available
		f.clearUploadDomainsCache()
		domains, err = f.getUploadDomains(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to get upload domains after cache clear: %w", err)
		}
		if len(domains) == 0 {
			return 0, errors.New("no upload domains available")
		}
	}

	// Use the first domain
	uploadDomain := domains[0]

	// Create multipart form data
	formData := url.Values{}
	formData.Set("parentFileID", toString(parentID))
	formData.Set("filename", filename)
	formData.Set("etag", md5Hash)
	formData.Set("size", strconv.FormatInt(size, 10))
	formData.Set("duplicate", "2") // Always overwrite existing files

	formReader, contentType, overhead, err := rest.MultipartUpload(ctx, bytes.NewReader(data), formData, "file", filename)
	if err != nil {
		return 0, fmt.Errorf("failed to create multipart upload: %w", err)
	}

	contentLength := overhead + int64(len(data))

	opts := rest.Opts{
		Method:        apiSingleUpload.method,
		RootURL:       uploadDomain,
		Path:          apiSingleUpload.uri,
		Body:          formReader,
		ContentType:   contentType,
		ContentLength: &contentLength,
	}

	resp := api.Response[api.SingleUploadResponse]{}

	_ = apiSingleUpload.limiter.Wait(ctx)
	_, err = f.srv.CallJSON(ctx, &opts, nil, &resp)
	if err != nil {
		return 0, err
	}

	if resp.Code != 0 {
		return 0, fmt.Errorf("failed to upload file: %d (%s)", resp.Code, resp.Message)
	}

	if !resp.Data.Completed {
		return 0, errors.New("single upload not completed")
	}

	return resp.Data.FileID, nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	panic("not impled")
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	pathID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Check if directory is empty
	files, err := f.listAll(ctx, toId(pathID))
	if err != nil {
		return fmt.Errorf("failed to list directory contents: %w", err)
	}

	if len(files) > 0 {
		return fs.ErrorDirectoryNotEmpty
	}

	// Delete the directory
	opts := rest.Opts{
		Method: apiFileTrash.method,
		Path:   apiFileTrash.uri,
	}

	request := api.FileTrashRequest{
		FileIDs: []int64{toId(pathID)},
	}

	resp := api.Response[api.FileTrashResponse]{}

	_ = apiFileTrash.limiter.Wait(ctx)
	_, err = f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 0 {
		return fmt.Errorf("failed to delete directory: %d (%s)", resp.Code, resp.Message)
	}

	// Remove from cache
	f.dirCache.FlushDir(dir)

	return nil
}

func toString(x int64) string {
	return strconv.FormatInt(x, 10)
}

func toId(x string) int64 {
	id, err := strconv.ParseInt(x, 10, 64)
	if err != nil {
		fs.Errorf(nil, "Failed to parse id: %v", err)
		return 0
	}
	return id
}
