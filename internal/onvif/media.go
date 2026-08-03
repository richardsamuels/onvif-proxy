package onvif

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Aiaid/onvif-proxy/internal/config"
	"github.com/Aiaid/onvif-proxy/internal/soap"
)

// handleMedia dispatches POST /onvif/media_service.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	req, err := parse(w, r)
	if err != nil {
		soap.WriteInvalidArg(w, "malformed SOAP request")
		return
	}
	if !s.authorized(req, req.Action) {
		soap.WriteNotAuthorized(w)
		return
	}

	switch req.Action {
	case "GetServiceCapabilities":
		write(w, s.mediaServiceCapabilities())
	case "GetProfiles":
		write(w, s.profiles())
	case "GetProfile":
		s.getProfile(w, req)
	case "GetVideoSources":
		write(w, s.videoSources())
	case "GetVideoSourceConfigurations":
		write(w, s.videoSourceConfigurations("GetVideoSourceConfigurationsResponse", "Configurations"))
	case "GetVideoSourceConfiguration":
		write(w, s.videoSourceConfigurations("GetVideoSourceConfigurationResponse", "Configuration"))
	case "GetCompatibleVideoSourceConfigurations":
		write(w, s.videoSourceConfigurations("GetCompatibleVideoSourceConfigurationsResponse", "Configurations"))
	case "GetVideoEncoderConfigurations":
		write(w, s.videoEncoderConfigurations("GetVideoEncoderConfigurationsResponse"))
	case "GetCompatibleVideoEncoderConfigurations":
		write(w, s.videoEncoderConfigurations("GetCompatibleVideoEncoderConfigurationsResponse"))
	case "GetVideoEncoderConfiguration":
		s.getVideoEncoderConfiguration(w, req)
	case "GetVideoEncoderConfigurationOptions":
		s.getVideoEncoderConfigurationOptions(w, req)
	case "SetVideoEncoderConfiguration":
		write(w, "<trt:SetVideoEncoderConfigurationResponse/>")
	case "GetStreamUri":
		s.getStreamUri(w, r, req)
	case "GetSnapshotUri":
		s.getSnapshotUri(w, r, req)
	case "GetGuaranteedNumberOfVideoEncoderInstances":
		write(w, s.guaranteedInstances())
	case "GetAudioSources":
		write(w, "<trt:GetAudioSourcesResponse></trt:GetAudioSourcesResponse>")
	case "GetAudioSourceConfigurations":
		write(w, "<trt:GetAudioSourceConfigurationsResponse></trt:GetAudioSourceConfigurationsResponse>")
	case "GetAudioEncoderConfigurations":
		write(w, "<trt:GetAudioEncoderConfigurationsResponse></trt:GetAudioEncoderConfigurationsResponse>")
	case "GetAudioDecoderConfigurations":
		write(w, "<trt:GetAudioDecoderConfigurationsResponse></trt:GetAudioDecoderConfigurationsResponse>")
	case "GetAudioOutputs":
		write(w, "<trt:GetAudioOutputsResponse></trt:GetAudioOutputsResponse>")
	case "GetAudioOutputConfigurations":
		write(w, "<trt:GetAudioOutputConfigurationsResponse></trt:GetAudioOutputConfigurationsResponse>")
	case "GetOSDs":
		write(w, "<trt:GetOSDsResponse></trt:GetOSDsResponse>")
	default:
		soap.WriteActionNotSupported(w, req.Action)
	}
}

func (s *Server) mediaServiceCapabilities() string {
	return "<trt:GetServiceCapabilitiesResponse>" +
		`<trt:Capabilities SnapshotUri="true" Rotation="false" VideoSourceMode="false" OSD="false">` +
		fmt.Sprintf(`<trt:ProfileCapabilities MaximumNumberOfProfiles="%d"/>`, len(s.dev.Streams)) +
		`<trt:StreamingCapabilities RTPMulticast="false" RTP_TCP="true" RTP_RTSP_TCP="true"/>` +
		"</trt:Capabilities></trt:GetServiceCapabilitiesResponse>"
}

// videoSourceConfig renders the shared VideoSourceConfiguration (token "vsc").
// The bounds are the full frame of the source, i.e. the primary stream.
func (s *Server) videoSourceConfig(tag string) string {
	p := s.dev.PrimaryStream()
	return fmt.Sprintf("<tt:%s token=\"vsc\">", tag) +
		"<tt:Name>vsc</tt:Name>" +
		fmt.Sprintf("<tt:UseCount>%d</tt:UseCount>", len(s.dev.Streams)) +
		"<tt:SourceToken>src</tt:SourceToken>" +
		fmt.Sprintf(`<tt:Bounds x="0" y="0" width="%d" height="%d"/>`, p.Width, p.Height) +
		fmt.Sprintf("</tt:%s>", tag)
}

// videoEncoderConfig renders a stream's VideoEncoderConfiguration node using
// the given element tag (Configuration / Configurations).
func videoEncoderConfig(tag string, st *config.Stream) string {
	gov := st.Framerate * 2
	if gov < 1 {
		gov = 1
	}
	codecNode := ""
	switch st.CodecName() {
	case "jpeg":
		// JPEG has no codec-specific configuration node in tt:VideoEncoderConfiguration.
	case "mpeg4":
		codecNode = fmt.Sprintf("<tt:MPEG4><tt:GovLength>%d</tt:GovLength><tt:Mpeg4Profile>SP</tt:Mpeg4Profile></tt:MPEG4>", gov)
	default:
		codecNode = fmt.Sprintf("<tt:H264><tt:GovLength>%d</tt:GovLength><tt:H264Profile>Main</tt:H264Profile></tt:H264>", gov)
	}
	return fmt.Sprintf("<tt:%s token=\"%s\">", tag, soap.XMLEscape(st.EncoderToken())) +
		"<tt:Name>" + soap.XMLEscape(st.EncoderToken()) + "</tt:Name>" +
		"<tt:UseCount>1</tt:UseCount>" +
		"<tt:Encoding>" + encoderName(st) + "</tt:Encoding>" +
		fmt.Sprintf("<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>", st.Width, st.Height) +
		"<tt:Quality>5</tt:Quality>" +
		"<tt:RateControl>" +
		fmt.Sprintf("<tt:FrameRateLimit>%d</tt:FrameRateLimit>", st.Framerate) +
		"<tt:EncodingInterval>1</tt:EncodingInterval>" +
		fmt.Sprintf("<tt:BitrateLimit>%d</tt:BitrateLimit>", st.Bitrate) +
		"</tt:RateControl>" +
		codecNode +
		"<tt:Multicast><tt:Address><tt:Type>IPv4</tt:Type><tt:IPv4Address>0.0.0.0</tt:IPv4Address></tt:Address><tt:Port>0</tt:Port><tt:TTL>1</tt:TTL><tt:AutoStart>false</tt:AutoStart></tt:Multicast>" +
		"<tt:SessionTimeout>PT10S</tt:SessionTimeout>" +
		fmt.Sprintf("</tt:%s>", tag)
}

func encoderName(st *config.Stream) string {
	switch st.CodecName() {
	case "jpeg":
		return "JPEG"
	case "mpeg4":
		return "MPEG4"
	default:
		return "H264"
	}
}

// profileBody renders the inner nodes (Name, VSC, VEC) shared by GetProfiles
// and GetProfile.
func (s *Server) profileBody(st *config.Stream) string {
	return "<tt:Name>" + soap.XMLEscape(st.ProfileToken()) + "</tt:Name>" +
		s.videoSourceConfig("VideoSourceConfiguration") +
		videoEncoderConfig("VideoEncoderConfiguration", st)
}

func (s *Server) profiles() string {
	var b strings.Builder
	b.WriteString("<trt:GetProfilesResponse>")
	for _, st := range s.dev.Streams {
		b.WriteString(fmt.Sprintf(`<trt:Profiles fixed="true" token="%s">`, soap.XMLEscape(st.ProfileToken())))
		b.WriteString(s.profileBody(st))
		b.WriteString("</trt:Profiles>")
	}
	b.WriteString("</trt:GetProfilesResponse>")
	return b.String()
}

func (s *Server) getProfile(w http.ResponseWriter, req *soap.Request) {
	token, _ := soap.ExtractElement(req.Body, "ProfileToken")
	st := s.dev.StreamByProfileToken(token)
	if st == nil {
		soap.WriteInvalidArg(w, "no such profile: "+token)
		return
	}
	body := "<trt:GetProfileResponse>" +
		fmt.Sprintf(`<trt:Profile fixed="true" token="%s">`, soap.XMLEscape(st.ProfileToken())) +
		s.profileBody(st) +
		"</trt:Profile></trt:GetProfileResponse>"
	write(w, body)
}

func (s *Server) videoSources() string {
	p := s.dev.PrimaryStream()
	return "<trt:GetVideoSourcesResponse>" +
		`<trt:VideoSources token="src">` +
		fmt.Sprintf("<tt:Framerate>%d</tt:Framerate>", p.Framerate) +
		fmt.Sprintf("<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>", p.Width, p.Height) +
		"</trt:VideoSources>" +
		"</trt:GetVideoSourcesResponse>"
}

func (s *Server) videoSourceConfigurations(responseTag, itemTag string) string {
	return fmt.Sprintf("<trt:%s>", responseTag) +
		s.videoSourceConfig(itemTag) +
		fmt.Sprintf("</trt:%s>", responseTag)
}

func (s *Server) videoEncoderConfigurations(responseTag string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<trt:%s>", responseTag))
	for _, st := range s.dev.Streams {
		b.WriteString(videoEncoderConfig("Configurations", st))
	}
	b.WriteString(fmt.Sprintf("</trt:%s>", responseTag))
	return b.String()
}

func (s *Server) getVideoEncoderConfiguration(w http.ResponseWriter, req *soap.Request) {
	token, _ := soap.ExtractElement(req.Body, "ConfigurationToken")
	for _, st := range s.dev.Streams {
		if st.EncoderToken() == token {
			write(w, "<trt:GetVideoEncoderConfigurationResponse>"+
				videoEncoderConfig("Configuration", st)+
				"</trt:GetVideoEncoderConfigurationResponse>")
			return
		}
	}
	soap.WriteInvalidArg(w, "no such configuration: "+token)
}

func (s *Server) getVideoEncoderConfigurationOptions(w http.ResponseWriter, req *soap.Request) {
	streams := s.dev.Streams
	if token, ok := soap.ExtractElement(req.Body, "ConfigurationToken"); ok && token != "" {
		streams = nil
		for _, st := range s.dev.Streams {
			if st.EncoderToken() == token {
				streams = []*config.Stream{st}
				break
			}
		}
		if len(streams) == 0 {
			soap.WriteInvalidArg(w, "no such configuration: "+token)
			return
		}
	} else if token, ok := soap.ExtractElement(req.Body, "ProfileToken"); ok && token != "" {
		st := s.dev.StreamByProfileToken(token)
		if st == nil {
			soap.WriteInvalidArg(w, "no such profile: "+token)
			return
		}
		streams = []*config.Stream{st}
	}
	write(w, videoEncoderConfigurationOptions(streams))
}

func videoEncoderConfigurationOptions(streams []*config.Stream) string {
	grouped := map[string][]*config.Stream{}
	for _, st := range streams {
		grouped[st.CodecName()] = append(grouped[st.CodecName()], st)
	}
	var options strings.Builder
	for _, codec := range []string{"jpeg", "mpeg4", "h264"} {
		if streams := grouped[codec]; len(streams) > 0 {
			options.WriteString(videoEncoderCodecOptions(codec, streams))
		}
	}
	return "<trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options>" +
		"<tt:QualityRange><tt:Min>1</tt:Min><tt:Max>10</tt:Max></tt:QualityRange>" +
		options.String() +
		"</trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse>"
}

func videoEncoderCodecOptions(codec string, streams []*config.Stream) string {
	var res strings.Builder
	maxFR := 0
	seenResolutions := map[[2]int]bool{}
	for _, st := range streams {
		size := [2]int{st.Width, st.Height}
		if !seenResolutions[size] {
			seenResolutions[size] = true
			res.WriteString(fmt.Sprintf("<tt:ResolutionsAvailable><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:ResolutionsAvailable>", st.Width, st.Height))
		}
		if st.Framerate > maxFR {
			maxFR = st.Framerate
		}
	}
	if maxFR < 1 {
		maxFR = 30
	}
	ranges := fmt.Sprintf("<tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>%d</tt:Max></tt:FrameRateRange>", maxFR) +
		"<tt:EncodingIntervalRange><tt:Min>1</tt:Min><tt:Max>1</tt:Max></tt:EncodingIntervalRange>"
	switch codec {
	case "jpeg":
		return "<tt:JPEG>" + res.String() + ranges + "</tt:JPEG>"
	case "mpeg4":
		return "<tt:MPEG4>" +
			res.String() +
			"<tt:GovLengthRange><tt:Min>1</tt:Min><tt:Max>250</tt:Max></tt:GovLengthRange>" +
			ranges +
			"<tt:Mpeg4ProfilesSupported>SP</tt:Mpeg4ProfilesSupported>" +
			"</tt:MPEG4>"
	default:
		return "<tt:H264>" +
			res.String() +
			"<tt:GovLengthRange><tt:Min>1</tt:Min><tt:Max>250</tt:Max></tt:GovLengthRange>" +
			ranges +
			"<tt:H264ProfilesSupported>Main</tt:H264ProfilesSupported>" +
			"</tt:H264>"
	}
}

func (s *Server) guaranteedInstances() string {
	counts := map[string]int{}
	for _, st := range s.dev.Streams {
		counts[encoderName(st)]++
	}
	var body strings.Builder
	body.WriteString("<trt:GetGuaranteedNumberOfVideoEncoderInstancesResponse>")
	body.WriteString(fmt.Sprintf("<trt:TotalNumber>%d</trt:TotalNumber>", len(s.dev.Streams)))
	for _, codec := range []string{"JPEG", "H264", "MPEG4"} {
		if count := counts[codec]; count > 0 {
			body.WriteString(fmt.Sprintf("<trt:%s>%d</trt:%s>", codec, count, codec))
		}
	}
	body.WriteString("</trt:GetGuaranteedNumberOfVideoEncoderInstancesResponse>")
	return body.String()
}

func mediaURIBody(responseTag, uri string) string {
	return fmt.Sprintf("<trt:%s>", responseTag) +
		"<trt:MediaUri>" +
		"<tt:Uri>" + soap.XMLEscape(uri) + "</tt:Uri>" +
		"<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>" +
		"<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>" +
		"<tt:Timeout>PT0S</tt:Timeout>" +
		"</trt:MediaUri>" +
		fmt.Sprintf("</trt:%s>", responseTag)
}

func (s *Server) getStreamUri(w http.ResponseWriter, r *http.Request, req *soap.Request) {
	token, _ := soap.ExtractElement(req.Body, "ProfileToken")
	st := s.dev.StreamByProfileToken(token)
	if st == nil {
		soap.WriteInvalidArg(w, "no such profile: "+token)
		return
	}
	// Credentials never appear in the advertised URI: it points at the local
	// RTSP proxy port, which forwards to the authenticated upstream.
	uri := fmt.Sprintf("rtsp://%s:%d%s", s.hostname(r), s.dev.ProxyPortFor(st), st.PathQuery())
	write(w, mediaURIBody("GetStreamUriResponse", uri))
}

func (s *Server) getSnapshotUri(w http.ResponseWriter, r *http.Request, req *soap.Request) {
	token, _ := soap.ExtractElement(req.Body, "ProfileToken")
	st := s.dev.StreamByProfileToken(token)
	if st == nil {
		soap.WriteInvalidArg(w, "no such profile: "+token)
		return
	}
	uri := fmt.Sprintf("http://%s/onvif/snapshot?token=%s", s.hostPort(r), st.ProfileToken())
	write(w, mediaURIBody("GetSnapshotUriResponse", uri))
}
