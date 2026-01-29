package tts

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "time"

    pb "yuzu/agent/internal/tts/pb"
)

type Server struct{ pb.UnimplementedTTSServer }

func NewServer() *Server { return &Server{} }

func (s *Server) Session(stream pb.TTS_SessionServer) error {
    parent := stream.Context()
    // Expect StartRequest then stream audio chunks
    msg, err := stream.Recv()
    if err != nil { return err }
    start := msg.GetStart()
    if start == nil { return fmt.Errorf("expected start request") }
    _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Connected{Connected: &pb.Connected{SessionId: start.GetSessionId()}}})

    apiKey := os.Getenv("ELEVENLABS_API_KEY")
    if apiKey == "" { _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{Code:"config", Message:"missing ELEVENLABS_API_KEY"}}}); return nil }

    // Build request to ElevenLabs (non-streaming REST)
    url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", start.GetVoiceId())
    body := map[string]any{"text": start.GetText()}
    reqBytes, _ := json.Marshal(body)
    req, err := http.NewRequestWithContext(parent, http.MethodPost, url, bytes.NewReader(reqBytes))
    if err != nil { return err }
    req.Header.Set("xi-api-key", apiKey)
    req.Header.Set("accept", "audio/wav")
    req.Header.Set("content-type", "application/json")
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 { b,_ := io.ReadAll(io.LimitReader(resp.Body,1024)); _ = stream.Send(&pb.ServerMessage{Msg:&pb.ServerMessage_Error{Error:&pb.Error{Code:"http", Message:fmt.Sprintf("status=%d body=%s",resp.StatusCode,string(b))}}}); return nil }

    // Decode WAV header and stream PCM16@48k 20ms frames
    pcm, err := readWAVPCM16(resp.Body)
    if err != nil { _ = stream.Send(&pb.ServerMessage{Msg:&pb.ServerMessage_Error{Error:&pb.Error{Code:"decode", Message:err.Error()}}}); return nil }
    frameBytes := 48000/50*2 // 20ms * 48000 * 2 bytes
    pos := 0
    for pos < len(pcm) {
        end := pos + frameBytes
        if end > len(pcm) { end = len(pcm) }
        chunk := pcm[pos:end]
        pos = end
        if err := stream.Send(&pb.ServerMessage{Msg:&pb.ServerMessage_Audio{Audio:&pb.AudioChunk{Pcm48K: chunk}}}); err != nil { return nil }
        time.Sleep(20*time.Millisecond)
    }
    return nil
}

// readWAVPCM16 is a small WAV parser that returns raw PCM16 bytes for mono (or averages stereo) at any sample rate.
// For simplicity we assume input WAV is 48kHz mono 16-bit; if stereo, we average channels.
func readWAVPCM16(r io.Reader) ([]byte, error) {
    // Minimal parser: read the full body; assume standard PCM header; find 'data' chunk.
    b, err := io.ReadAll(r)
    if err != nil { return nil, err }
    if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" { return nil, fmt.Errorf("not a WAV") }
    off := 12
    var dataOff, dataLen int
    var fmtCh uint16
    var sampRate uint32
    var bits uint16
    for off+8 <= len(b) {
        cid := string(b[off:off+4])
        csz := int(uint32(b[off+4]) | uint32(b[off+5])<<8 | uint32(b[off+6])<<16 | uint32(b[off+7])<<24)
        off += 8
        if cid == "fmt " {
            if off+csz > len(b) { return nil, fmt.Errorf("bad fmt chunk") }
            fmtTag := uint16(b[off]) | uint16(b[off+1])<<8
            fmtCh = uint16(b[off+2]) | uint16(b[off+3])<<8
            sampRate = uint32(b[off+4]) | uint32(b[off+5])<<8 | uint32(b[off+6])<<16 | uint32(b[off+7])<<24
            bits = uint16(b[off+14]) | uint16(b[off+15])<<8
            if fmtTag != 1 || bits != 16 { return nil, fmt.Errorf("unsupported WAV format") }
            off += csz
        } else if cid == "data" {
            dataOff = off
            dataLen = csz
            break
        } else {
            off += csz
        }
    }
    if dataOff <= 0 || dataOff+dataLen > len(b) { return nil, fmt.Errorf("no data chunk") }
    raw := b[dataOff : dataOff+dataLen]
    // if stereo, average to mono
    if fmtCh == 2 {
        // simple average of int16 pairs
        out := make([]byte, dataLen/2)
        for i := 0; i+3 < len(raw); i += 4 {
            // little endian samples
            a := int16(uint16(raw[i]) | uint16(raw[i+1])<<8)
            c := int16(uint16(raw[i+2]) | uint16(raw[i+3])<<8)
            avg := int32(a)+int32(c)
            avg /= 2
            u := uint16(int16(avg))
            j := (i/2)
            out[j] = byte(u & 0xFF)
            out[j+1] = byte(u >> 8)
        }
        raw = out
    }
    // For simplicity, assume sampRate is already 48000; otherwise caller should resample upstream
    _ = sampRate
    return raw, nil
}

