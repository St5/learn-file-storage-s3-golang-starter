package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	http.MaxBytesReader(w, r.Body, 1<<30)

	//Get videoID from URL
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}
	// Authenticate user
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

	// Load video from database
	videoDb, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	// Check if user owns video
	if videoDb.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User does not own video", nil)
		return
	}

	// Upload video to memory
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	// Check if is file mp4 video
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}
	//Save file in tempory folder
	tmpFile, err := os.CreateTemp("","video.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_,err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save file", err)
		return
	}

	//reset pointer to start of file
	tmpFile.Seek(0,io.SeekStart)


	//Choose prefix/folder for S3
	prefix := "other"
	aspectRation, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	switch aspectRation {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	}

	//Move header to start of file
	processedFileName, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(processedFileName)
	processedFile, err := os.OpenFile(processedFileName, os.O_RDONLY, 0666)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}

	
	//Upload video to S3
	randomBites := make([]byte, 32)
	_, err = rand.Read(randomBites)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes", err)
		return
	}
	name :=base64.URLEncoding.EncodeToString(randomBites)
	fileName := prefix + "/" + name + ".mp4"
	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key: &fileName,
		Body: processedFile,
		ContentType: &mediaType,
	} )
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file to S3", err)
		return
	}

	//Update video in database
	videoUrl := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, fileName)
	//videoUrl := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileName)
	videoDb.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoDb)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	// videoDb, err = cfg.dbVideoToSignedVideo(videoDb)
	// if err != nil {
	// 	respondWithError(w, http.StatusInternalServerError, "Couldn't sign video", err)
	// 	return
	// }

	respondWithJSON(w, http.StatusOK, videoDb)

}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignResult, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return presignResult.URL, nil
}

func getVideoAspectRatio(filePath string) (string, error){
	//Run ffprobe to get video metadata
	command := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out strings.Builder
	command.Stdout = &out

	err := command.Run()
	if err != nil {
		return "", err
	}

	//Parse ffprobe output
	var ffprobeOutput struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
			DisplayAspectRatio string `json:"display_aspect_ratio"`

		} `json:"streams"`
	}
	err = json.Unmarshal([]byte(out.String()), &ffprobeOutput)
	if err != nil {
		return "", err
	}

	//Return aspect ratio
	if len(ffprobeOutput.Streams) == 0 {
		return "", errors.New("No streams found")
	}
	return ffprobeOutput.Streams[0].DisplayAspectRatio, nil
}

/**
 * Process video for fast start
 * Convert video file with meta data from the end of the file to the beginning
 */
func processVideoForFastStart(filePath string) (string, error) {
	tmpName := filePath + ".processing"

	command := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", tmpName)
	err := command.Run()
	if err != nil {
		return "", err
	}
	return tmpName, nil
}


