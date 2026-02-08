package stt

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    metricAudioBytes = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_audio_bytes_total",
        Help: "Total audio bytes enqueued to provider",
    })

    metricFrames = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_frames_total",
        Help: "Total audio frames enqueued to provider",
    })

    metricDrops = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_drops_total",
        Help: "Total audio frames dropped due to backpressure",
    })

    metricReconnects = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_reconnects_total",
        Help: "Total reconnects to provider",
    })

    metricCircuitOpens = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_circuit_open_total",
        Help: "Circuit breaker open events",
    })

    metricConnectMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "stt_connect_ms",
        Help:    "Time to establish provider connection (ms)",
        Buckets: prometheus.ExponentialBuckets(10, 1.8, 10),
    })

    metricTTFTMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "stt_ttft_ms",
        Help:    "Time to first token (ms)",
        Buckets: prometheus.ExponentialBuckets(50, 1.6, 10),
    })

    metricFinalLatencyMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "stt_final_latency_ms",
        Help:    "Latency from drain to final transcript (ms)",
        Buckets: prometheus.ExponentialBuckets(100, 1.6, 10),
    })

    gaugeSessions = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "stt_sessions_active",
        Help: "Active STT sessions",
    })

    gaugeQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "stt_send_queue_depth",
        Help: "Current depth of provider send queue (last observed)",
    })

    // Transcript handling metrics
    metricFinalEmitted = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "stt_final_emitted_total",
        Help: "Final transcripts emitted by source (provider, provider_cached, interim_fallback)",
    }, []string{"source"})

    metricEmptyFinalSkipped = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_empty_final_skipped_total",
        Help: "Empty final transcripts skipped",
    })

    // Utterance boundary metrics
    metricUtteranceEvents = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "stt_utterance_events_total",
        Help: "Utterance boundary events observed",
    }, []string{"type"}) // speech_started, utterance_end

    // Event channel drops
    metricEventDrops = promauto.NewCounter(prometheus.CounterOpts{
        Name: "stt_event_drops_total",
        Help: "Events dropped due to slow consumer (channel backpressure)",
    })
)
