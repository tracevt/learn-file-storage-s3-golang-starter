package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type Stream struct {
	Width              int    `json:"width,omitempty"`
	Height             int    `json:"height,omitempty"`
	CodedWidth         int    `json:"coded_width,omitempty"`
	CodedHeight        int    `json:"coded_height,omitempty"`
	SampleAspectRatio  string `json:"sample_aspect_ratio,omitempty"`
	DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
}

type MediaInfo struct {
	StreamsStream []Stream `json:"streams"`
}

func unmarshalJSON(buf *bytes.Buffer) (MediaInfo, error) {
	var mediaInfo MediaInfo
	decoder := json.NewDecoder(buf)
	err := decoder.Decode(&mediaInfo)
	if err != nil {
		return MediaInfo{}, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return mediaInfo, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer

	err := cmd.Run()
	if err != nil {
		fmt.Println(err)
		return "", err
	}

	mediaInfo, err := unmarshalJSON(&buffer)
	if err != nil {
		return "", err
	}

	fmt.Printf("mediaInfo %s \n", mediaInfo.StreamsStream[0].DisplayAspectRatio)

	return mediaInfo.StreamsStream[0].DisplayAspectRatio, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", processedFilePath)
	var output strings.Builder
	cmd.Stdout = &output
	err := cmd.Run()

	if err != nil {
		fmt.Println(err)
		return "", err
	}

	return processedFilePath, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxVideoSize = 10 << 30 // 1GB
	r.ParseMultipartForm(maxVideoSize)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Video ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate auth", err)
		return
	}

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get the video metadata", err)
		return
	}

	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Couldnñt validate metadata", err)
		return
	}

	video, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse the provided video", err)
		return
	}

	defer video.Close()

	videoTypeHeader := header.Header.Get("Content-Type")
	videoType, _, err := mime.ParseMediaType(videoTypeHeader)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse the file type submitted", err)
		return
	}

	if videoType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only MP4 videos are allowed", err)
		return
	}

	tempVideoFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save the video file", err)
		return
	}

	defer os.Remove(tempVideoFile.Name())
	defer tempVideoFile.Close() // This is a LIFO operation

	if _, err := io.Copy(tempVideoFile, video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save the video to a temp file", err)
		return
	}

	tempVideoFile.Seek(0, io.SeekStart) // Reset the file pointer to the beginning

	// Get the aspect ratio
	aspectRatio, err := getVideoAspectRatio(tempVideoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't determine the aspect ratio of the video", err)
	}
	var ratioPrefix string
	if aspectRatio == "16:9" {
		ratioPrefix = "landscape"
	} else if aspectRatio == "9:16" {
		ratioPrefix = "portrait"
	} else {
		ratioPrefix = "other"
	}

	fmt.Printf("ratioPrefix %s \n", ratioPrefix)

	// Make a processed video
	processedVideoFilePath, err := processVideoForFastStart(tempVideoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create the processed version of the video", err)
	}

	processedVideo, err := os.Open(processedVideoFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open the processed video for uploading", err)
	}

	defer os.Remove(processedVideo.Name())
	defer processedVideo.Close()

	// Create a unique file name for the AWS bucket
	sliceLength := 32
	randomBytes := make([]byte, sliceLength)

	if _, err := rand.Read(randomBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random filename", err)
		return
	}

	var urlEncoder = base64.URLEncoding.WithPadding(base64.NoPadding)
	fileNamePrefix := urlEncoder.EncodeToString(randomBytes)

	bucketFileKey := filepath.Join(fmt.Sprintf("%s/%s.%s", ratioPrefix, fileNamePrefix, strings.Split(videoType, "/")[1]))
	fmt.Printf("filename: %s \n", bucketFileKey)

	// Put it into the s3 bucket for real
	// Set PutObject config options
	s3InputOption := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &bucketFileKey,
		Body:        processedVideo,
		ContentType: &videoType,
	}

	if _, err := cfg.s3Client.PutObject(context.TODO(), &s3InputOption); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't send video the AWS", err)
		return
	}

	videoAwsUrl := fmt.Sprintf("https://%s.cloudfront.net/%s", cfg.s3CfDistribution, bucketFileKey)
	videoMetadata.VideoURL = &videoAwsUrl

	if err = cfg.db.UpdateVideo(videoMetadata); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update the Video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}
