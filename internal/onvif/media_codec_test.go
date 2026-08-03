package onvif

import (
	"strings"
	"testing"

	"github.com/Aiaid/onvif-proxy/internal/config"
)

func codecTestStreams() []*config.Stream {
	return []*config.Stream{
		{Name: "main", Codec: "h264", Width: 1920, Height: 1080, Framerate: 25, Bitrate: 4096},
		{Name: "still", Codec: "jpeg", Width: 1280, Height: 720, Framerate: 10, Bitrate: 1024},
		{Name: "legacy", Codec: "mpeg4", Width: 640, Height: 480, Framerate: 15, Bitrate: 512},
	}
}

func TestVideoEncoderConfigurationsDeclareConfiguredCodec(t *testing.T) {
	streams := codecTestStreams()
	for _, tt := range []struct {
		stream       *config.Stream
		encoding     string
		specificNode string
	}{
		{streams[0], "H264", "<tt:H264>"},
		{streams[1], "JPEG", ""},
		{streams[2], "MPEG4", "<tt:MPEG4>"},
	} {
		body := videoEncoderConfig("Configuration", tt.stream)
		if !strings.Contains(body, "<tt:Encoding>"+tt.encoding+"</tt:Encoding>") {
			t.Errorf("%s configuration has wrong encoding:\n%s", tt.stream.Name, body)
		}
		if tt.specificNode != "" && !strings.Contains(body, tt.specificNode) {
			t.Errorf("%s configuration missing %s:\n%s", tt.stream.Name, tt.specificNode, body)
		}
	}

	jpeg := videoEncoderConfig("Configuration", streams[1])
	if strings.Contains(jpeg, "<tt:H264>") || strings.Contains(jpeg, "<tt:MPEG4>") {
		t.Fatalf("JPEG configuration contains another codec's node:\n%s", jpeg)
	}
}

func TestVideoEncoderOptionsFollowConfiguredCodecs(t *testing.T) {
	streams := codecTestStreams()
	all := videoEncoderConfigurationOptions(streams)
	for _, node := range []string{"<tt:JPEG>", "<tt:H264>", "<tt:MPEG4>"} {
		if !strings.Contains(all, node) {
			t.Errorf("generic options missing %s:\n%s", node, all)
		}
	}

	jpegOnly := videoEncoderConfigurationOptions([]*config.Stream{streams[1]})
	if !strings.Contains(jpegOnly, "<tt:JPEG>") ||
		strings.Contains(jpegOnly, "<tt:H264>") ||
		strings.Contains(jpegOnly, "<tt:MPEG4>") {
		t.Fatalf("configuration-specific options advertise wrong codecs:\n%s", jpegOnly)
	}
}

func TestGuaranteedEncoderInstancesCountPerCodec(t *testing.T) {
	s := &Server{dev: &config.Device{Streams: codecTestStreams()}}
	body := s.guaranteedInstances()
	for _, want := range []string{
		"<trt:TotalNumber>3</trt:TotalNumber>",
		"<trt:JPEG>1</trt:JPEG>",
		"<trt:H264>1</trt:H264>",
		"<trt:MPEG4>1</trt:MPEG4>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guaranteed instances missing %s:\n%s", want, body)
		}
	}
}
