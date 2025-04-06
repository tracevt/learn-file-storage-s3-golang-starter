package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	thumbnail, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer thumbnail.Close()

	mediaTypeHeader := header.Header.Get("Content-Type")

	// Only allow pngs and jpegs
	mediatype, _, err := mime.ParseMediaType(mediaTypeHeader)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse the file type submitted", err)
		return
	}

	if mediatype != "image/jpeg" && mediatype != "image/png" {
		respondWithError(w, http.StatusBadRequest, "The file submitted is not an image", err)
		return
	}

	metadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get the video metadata", err)
		return
	}

	if metadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate metadata", err)
		return
	}

	// Store it on disk
	// 1. Create a random byte array
	sliceLength := 32
	randomBytes := make([]byte, sliceLength)

	if _, err := rand.Read(randomBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random filename", err)
		return
	}

	// 2. Create unique filename
	var urlEncoder = base64.URLEncoding.WithPadding(base64.NoPadding)
	fileNamePrefix := urlEncoder.EncodeToString(randomBytes)

	filename := filepath.Join(cfg.assetsRoot, fmt.Sprintf("%s.%s", fileNamePrefix, strings.Split(mediatype, "/")[1]))
	fmt.Printf("filename: %s \n", filename)

	// 3. Create the file on disk
	imageFile, err := os.Create(filename)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create the thumbnail file", err)
		return
	}

	if _, err := io.Copy(imageFile, thumbnail); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save the thumbnail to disk", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/%s", cfg.port, filename)
	metadata.ThumbnailURL = &thumbnailURL

	err = cfg.db.UpdateVideo(metadata)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't updated the Video Thumbnail", err)
		return
	}

	respondWithJSON(w, http.StatusOK, metadata)
}
