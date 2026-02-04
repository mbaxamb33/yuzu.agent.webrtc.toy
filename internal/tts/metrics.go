package tts

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    ttsSynthesisTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "tts_synthesis_total",
        Help: "Total TTS synthesis requests by status",
    }, []string{"status"})

    ttsFirstFrameMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "tts_first_frame_ms",
        Help:    "Latency from request start to first audio frame sent",
        Buckets: prometheus.ExponentialBuckets(20, 1.6, 10),
    })

    ttsTotalDurationMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "tts_total_duration_ms",
        Help:    "Total TTS synthesis time in milliseconds",
        Buckets: prometheus.ExponentialBuckets(50, 1.6, 12),
    })

    ttsElevenLabsLatencyMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "tts_elevenlabs_latency_ms",
        Help:    "Latency of ElevenLabs API response (first byte)",
        Buckets: prometheus.ExponentialBuckets(20, 1.6, 10),
    })
)

