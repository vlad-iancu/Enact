package enactmain

import (
	"fmt"
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/requesthelper"
)

// maxAvatarBytes caps avatar uploads; avatars are small images.
const maxAvatarBytes = 5 << 20 // 5 MiB

// avatarImageTypes maps sniffed content types to storage key extensions.
// The type is detected from the bytes (http.DetectContentType), never
// trusted from the filename.
var avatarImageTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// avatarURL resolves a user's avatar storage key to its public URL: the
// CloudFront distribution when configured, else the direct S3 URL. Keys
// contain UUIDs, so URLs are unguessable without being access-controlled.
func (a *MainAPI) avatarURL(avatarKey string) string {
	if avatarKey == "" {
		return ""
	}
	if a.cdn.Configured() {
		return a.cdn.URL(avatarKey)
	}
	return a.storage.ObjectURL(avatarKey)
}

// uploadAvatar stores a new avatar for the logged-in user: sniff + validate
// the image, upload under an unguessable UUID key, point the user record at
// it, then best-effort delete the previous object. Ordering matters — the
// record is only updated after the new object exists, so a failure never
// leaves it pointing at nothing.
func (a *MainAPI) uploadAvatar(req *restful.Request, resp *restful.Response) {
	sess, ok := a.session(req)
	if !ok {
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return
	}
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("avatar upload requested")

	files, status, msg := requesthelper.ReadUploadedFiles(req, resp, "file", maxAvatarBytes)
	if status != 0 {
		logger.Warn("invalid avatar upload", "err", msg)
		requesthelper.WriteError(req, resp, status, msg)
		return
	}
	if len(files) != 1 {
		logger.Warn("avatar upload with wrong file count", "files", len(files))
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "exactly one file must be uploaded")
		return
	}
	file := files[0]

	contentType := http.DetectContentType(file.Content)
	ext, ok := avatarImageTypes[contentType]
	if !ok {
		logger.Warn("avatar upload with unsupported content type", "content_type", contentType, "file_name", file.Filename)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("unsupported image type %q (supported: jpeg, png, webp, gif)", contentType))
		return
	}
	logger.Info("avatar validated", "content_type", contentType, "size_bytes", len(file.Content))

	user, found, err := a.users.GetByEmail(req.Request.Context(), sess.Email)
	if err != nil || !found {
		logger.Error("failed to load user for avatar upload", "found", found, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load user")
		return
	}
	previousKey := user.AvatarKey

	key := fmt.Sprintf("avatars/%s.%s", uuid.NewString(), ext)
	if err := a.storage.PutObject(req.Request.Context(), key, contentType, file.Content); err != nil {
		logger.Error("failed to store avatar", "key", key, "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to store avatar")
		return
	}
	logger.Info("avatar stored", "key", key)

	user.AvatarKey = key
	user.UpdatedAt = time.Now().UTC()
	if err := a.users.Save(req.Request.Context(), user); err != nil {
		logger.Error("failed to save user avatar key", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to save avatar")
		return
	}
	logger.Info("user avatar updated")

	// The old object is unreachable now (nothing references its key); a
	// failed delete only leaks storage, so it must not fail the request.
	if previousKey != "" {
		if err := a.storage.DeleteObject(req.Request.Context(), previousKey); err != nil {
			logger.Warn("failed to delete previous avatar", "key", previousKey, "err", err)
		} else {
			logger.Info("previous avatar deleted", "key", previousKey)
		}
	}

	requesthelper.WriteJSON(req, resp, http.StatusOK, a.toUserResponse(user))
}
