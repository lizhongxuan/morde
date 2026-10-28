package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"

	"morde/server/internal/app"
)

const (
	cloudDSHMaxImageBytes      = 512 << 10
	cloudDSHMaxToolImageCount  = 1
	cloudDSHMaxModelImageCount = 4
	cloudDSHMaxModelImageBytes = 2 << 20
	// shortcut: cap replay cache at 8 MiB per task; use durable bounded attachments if tasks need longer image histories.
	cloudDSHMaxImageCacheBytes = 8 << 20
	cloudDSHMaxImageWidth      = 1280
	cloudDSHMaxImageHeight     = 720
)

type cloudDSHToolImage struct {
	AttachmentID string `json:"attachment_id"`
	MediaType    string `json:"media_type"`
	Bytes        int    `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	DataBase64   string `json:"data_base64"`
}

type cloudDSHImageAttachment struct {
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	Bytes        int    `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	DataBase64   string `json:"data_base64"`
}

type cloudDSHImageRef struct {
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	Bytes        int    `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

type cloudDSHImage struct {
	mediaType string
	width     int
	height    int
	data      []byte
}

type cloudDSHImageRegistry struct {
	fence  app.ModelTaskFence
	images map[string]cloudDSHImage
	bytes  int
}

func validCloudDSHToolImageResult(name string, result app.AgentToolResult) bool {
	if result.Status != app.AgentToolSucceeded {
		return len(result.Images) == 0
	}
	if name != "desktop_screenshot" && name != "browser_screenshot" {
		return len(result.Images) == 0
	}
	return len(result.Images) == 1 && result.Images[0].MediaType == "image/jpeg" &&
		len(result.Images[0].Data) >= 4 && len(result.Images[0].Data) <= cloudDSHMaxImageBytes
}

func newCloudDSHImageRegistry(fence app.ModelTaskFence) *cloudDSHImageRegistry {
	return &cloudDSHImageRegistry{fence: fence, images: make(map[string]cloudDSHImage)}
}

func cloudDSHImageReferences(images []cloudDSHToolImage) []cloudDSHImageRef {
	refs := make([]cloudDSHImageRef, 0, len(images))
	for _, image := range images {
		refs = append(refs, cloudDSHImageRef{AttachmentID: image.AttachmentID, MediaType: image.MediaType,
			Bytes: image.Bytes, Width: image.Width, Height: image.Height})
	}
	return refs
}

func decodeCloudDSHImageRef(raw []byte) (cloudDSHImageRef, error) {
	var ref cloudDSHImageRef
	if len(raw) == 0 || !uniqueCloudDSHJSON(raw) {
		return ref, errors.New("dsh image reference is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ref) != nil || decoder.Decode(new(any)) != io.EOF || !validCloudDSHImageID(ref.AttachmentID) ||
		ref.MediaType != "image/jpeg" || ref.Bytes < 4 || ref.Bytes > cloudDSHMaxImageBytes ||
		ref.Width < 1 || ref.Width > cloudDSHMaxImageWidth || ref.Height < 1 || ref.Height > cloudDSHMaxImageHeight {
		return cloudDSHImageRef{}, errors.New("dsh image reference is invalid")
	}
	return ref, nil
}

func sameCloudDSHImageRefs(left, right []cloudDSHImageRef) bool {
	if len(left) != len(right) {
		return false
	}
	for index, image := range left {
		if image != right[index] {
			return false
		}
	}
	return true
}

func (r *cloudDSHImageRegistry) add(images []app.ModelImage) ([]cloudDSHToolImage, error) {
	if len(images) > cloudDSHMaxToolImageCount {
		return nil, errors.New("dsh tool returned too many images")
	}
	result := make([]cloudDSHToolImage, 0, len(images))
	for _, image := range images {
		width, height, err := validCloudDSHJPEG(image.MediaType, image.Data)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(image.Data)
		id := "morde_img_" + hex.EncodeToString(digest[:])
		if _, exists := r.images[id]; !exists {
			if r.bytes+len(image.Data) > cloudDSHMaxImageCacheBytes {
				return nil, errors.New("dsh task image storage limit exceeded")
			}
			r.images[id] = cloudDSHImage{mediaType: image.MediaType, width: width, height: height, data: append([]byte(nil), image.Data...)}
			r.bytes += len(image.Data)
		}
		result = append(result, cloudDSHToolImage{AttachmentID: id, MediaType: image.MediaType, Bytes: len(image.Data),
			Width: width, Height: height, DataBase64: base64.StdEncoding.EncodeToString(image.Data)})
	}
	return result, nil
}

func (r *cloudDSHImageRegistry) ResolveTaskModelImage(_ context.Context, fence app.ModelTaskFence, raw json.RawMessage) (app.ModelImage, error) {
	if fence.TaskID != r.fence.TaskID || fence.OwnerID != r.fence.OwnerID || fence.LeaseOwner != r.fence.LeaseOwner ||
		fence.LeaseVersion != r.fence.LeaseVersion || fence.AgentInstanceID != r.fence.AgentInstanceID ||
		fence.AgentGeneration != r.fence.AgentGeneration || fence.AgentSessionID != r.fence.AgentSessionID ||
		fence.AuthorizationVersion != r.fence.AuthorizationVersion || fence.ThroughSequence < 0 ||
		fence.EngineVersion != r.fence.EngineVersion || fence.ProfileVersion != r.fence.ProfileVersion {
		return app.ModelImage{}, errors.New("dsh image task fence changed")
	}
	attachment, data, err := decodeCloudDSHImageAttachment(raw)
	if err != nil {
		return app.ModelImage{}, err
	}
	registered, ok := r.images[attachment.AttachmentID]
	if !ok || registered.mediaType != attachment.MediaType || registered.width != attachment.Width ||
		registered.height != attachment.Height || len(registered.data) != attachment.Bytes || !bytes.Equal(registered.data, data) {
		return app.ModelImage{}, errors.New("dsh image is not backed by an authenticated Host result")
	}
	return app.ModelImage{MediaType: registered.mediaType, Data: append([]byte(nil), registered.data...)}, nil
}

func decodeCloudDSHImageAttachment(raw []byte) (cloudDSHImageAttachment, []byte, error) {
	var attachment cloudDSHImageAttachment
	invalid := errors.New("dsh image attachment is invalid")
	if len(raw) == 0 || !uniqueCloudDSHJSON(raw) {
		return attachment, nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&attachment) != nil || decoder.Decode(new(any)) != io.EOF ||
		!validCloudDSHImageID(attachment.AttachmentID) || attachment.MediaType != "image/jpeg" ||
		attachment.Bytes < 4 || attachment.Bytes > cloudDSHMaxImageBytes || attachment.Width < 1 ||
		attachment.Width > cloudDSHMaxImageWidth || attachment.Height < 1 || attachment.Height > cloudDSHMaxImageHeight ||
		attachment.DataBase64 == "" || len(attachment.DataBase64) > 700<<10 {
		return cloudDSHImageAttachment{}, nil, invalid
	}
	data, err := base64.StdEncoding.DecodeString(attachment.DataBase64)
	if err != nil || len(data) != attachment.Bytes || base64.StdEncoding.EncodeToString(data) != attachment.DataBase64 {
		return cloudDSHImageAttachment{}, nil, invalid
	}
	width, height, err := validCloudDSHJPEG(attachment.MediaType, data)
	if err != nil || width != attachment.Width || height != attachment.Height {
		return cloudDSHImageAttachment{}, nil, invalid
	}
	digest := sha256.Sum256(data)
	if attachment.AttachmentID != "morde_img_"+hex.EncodeToString(digest[:]) {
		return cloudDSHImageAttachment{}, nil, invalid
	}
	return attachment, data, nil
}

func validCloudDSHJPEG(mediaType string, data []byte) (int, int, error) {
	if mediaType != "image/jpeg" || len(data) < 4 || len(data) > cloudDSHMaxImageBytes {
		return 0, 0, errors.New("dsh image is outside the JPEG size limit")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Width > cloudDSHMaxImageWidth || config.Height < 1 || config.Height > cloudDSHMaxImageHeight {
		return 0, 0, errors.New("dsh JPEG dimensions are invalid")
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return 0, 0, errors.New("dsh JPEG data is invalid")
	}
	return config.Width, config.Height, nil
}

func validCloudDSHImageID(value string) bool {
	if len(value) != len("morde_img_")+sha256.Size*2 || value[:len("morde_img_")] != "morde_img_" {
		return false
	}
	for _, character := range value[len("morde_img_"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
