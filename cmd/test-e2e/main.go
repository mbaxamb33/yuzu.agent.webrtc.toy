package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	pb "yuzu/agent/internal/orchestrator/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	orchAddr := flag.String("orch", ":9090", "Orchestrator gRPC address")
	sessionID := flag.String("session", "test-e2e-"+time.Now().Format("150405"), "Session ID")
	text := flag.String("text", "Hello, how are you today?", "Text to send as transcript")
	timeout := flag.Duration("timeout", 30*time.Second, "Timeout for receiving responses")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Connect to Orchestrator
	conn, err := grpc.DialContext(ctx, *orchAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial orchestrator: %v", err)
	}
	defer conn.Close()

	client := pb.NewGatewayControlClient(conn)
	stream, err := client.Session(ctx)
	if err != nil {
		log.Fatalf("open session: %v", err)
	}

	// Start receiver goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			cmd, err := stream.Recv()
			if err == io.EOF {
				fmt.Println("\n[stream] EOF")
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				fmt.Printf("\n[stream] recv error: %v\n", err)
				return
			}
			printCommand(cmd)
		}
	}()

	fmt.Printf("=== E2E Internal Test ===\n")
	fmt.Printf("Session: %s\n", *sessionID)
	fmt.Printf("Text: %q\n\n", *text)

	// Step 1: Send SessionOpen
	fmt.Println("[1] Sending SessionOpen...")
	err = stream.Send(&pb.GatewayEvent{
		SessionId: *sessionID,
		Evt: &pb.GatewayEvent_SessionOpen{
			SessionOpen: &pb.SessionOpen{
				SessionId: *sessionID,
				RoomUrl:   "test://e2e",
			},
		},
	})
	if err != nil {
		log.Fatalf("send session_open: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Step 2: Send VADStart (simulating user started speaking)
	fmt.Println("[2] Sending VADStart...")
	err = stream.Send(&pb.GatewayEvent{
		SessionId: *sessionID,
		Evt: &pb.GatewayEvent_VadStart{
			VadStart: &pb.VADStart{TsMs: uint64(time.Now().UnixMilli())},
		},
	})
	if err != nil {
		log.Fatalf("send vad_start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Step 3: Send VADEnd (simulating user stopped speaking)
	fmt.Println("[3] Sending VADEnd...")
	err = stream.Send(&pb.GatewayEvent{
		SessionId: *sessionID,
		Evt: &pb.GatewayEvent_VadEnd{
			VadEnd: &pb.VADEnd{TsMs: uint64(time.Now().UnixMilli())},
		},
	})
	if err != nil {
		log.Fatalf("send vad_end: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Step 4: Send TranscriptFinal (this triggers LLM)
	fmt.Printf("[4] Sending TranscriptFinal: %q\n", *text)
	err = stream.Send(&pb.GatewayEvent{
		SessionId: *sessionID,
		Evt: &pb.GatewayEvent_TranscriptFinal{
			TranscriptFinal: &pb.TranscriptFinal{
				UtteranceId: "utt-1",
				Text:        *text,
			},
		},
	})
	if err != nil {
		log.Fatalf("send transcript_final: %v", err)
	}

	fmt.Println("\n[*] Waiting for OrchestratorCommands (StartTTS expected)...")
	fmt.Println("    Press Ctrl+C to exit or wait for timeout\n")

	// Wait for responses or timeout
	select {
	case <-done:
		fmt.Println("[*] Stream closed")
	case <-ctx.Done():
		fmt.Println("[*] Timeout reached")
	case <-waitForSignal():
		fmt.Println("[*] Interrupted")
	}

	os.Exit(0)
}

func printCommand(cmd *pb.OrchestratorCommand) {
	ts := time.Now().Format("15:04:05.000")
	switch c := cmd.Cmd.(type) {
	case *pb.OrchestratorCommand_StartTts:
		fmt.Printf("[%s] <- StartTTS: %q\n", ts, c.StartTts.GetText())
	case *pb.OrchestratorCommand_StopTts:
		fmt.Printf("[%s] <- StopTTS: reason=%s\n", ts, c.StopTts.GetReason())
	case *pb.OrchestratorCommand_StartMicToStt:
		fmt.Printf("[%s] <- StartMicToSTT\n", ts)
	case *pb.OrchestratorCommand_StopMicToStt:
		fmt.Printf("[%s] <- StopMicToSTT\n", ts)
	case *pb.OrchestratorCommand_ArmBargeIn:
		fmt.Printf("[%s] <- ArmBargeIn: guard_ms=%d min_rms=%d\n", ts, c.ArmBargeIn.GetGuardMs(), c.ArmBargeIn.GetMinRms())
	case *pb.OrchestratorCommand_Ack:
		fmt.Printf("[%s] <- Ack: %s\n", ts, c.Ack.GetInfo())
	default:
		fmt.Printf("[%s] <- Unknown command: %T\n", ts, c)
	}
}

func waitForSignal() chan struct{} {
	ch := make(chan struct{})
	go func() {
		// Simple blocking - let context timeout handle it
		select {}
	}()
	return ch
}
