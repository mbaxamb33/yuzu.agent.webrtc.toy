package orchestrator

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    metricVADFeatures = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_vad_features_total",
        Help: "Total VAD feature frames processed",
    })

    metricVADStarts = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_vad_starts_total",
        Help: "Total VAD speech start events",
    })

    metricVADEnds = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_vad_ends_total",
        Help: "Total VAD speech end events",
    })

    metricBargeIn = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_barge_in_events_total",
        Help: "Total barge-in stop events triggered",
    })

    metricBargeInGuardBlocks = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_barge_in_guard_blocks_total",
        Help: "Frames above threshold blocked by guard window",
    })

    metricBargeInLatency = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "orch_barge_in_latency_ms",
        Help:    "Latency from guard end to detected speech start",
        Buckets: prometheus.ExponentialBuckets(10, 1.6, 10),
    })

    metricTTSFirstAudio = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "orch_tts_first_audio_ms",
        Help:    "Latency from TTS start to first audio",
        Buckets: prometheus.ExponentialBuckets(50, 1.6, 10),
    })

    metricStateTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "orch_state_transitions_total",
        Help: "Orchestrator state transitions",
    }, []string{"from","to"})

    // Agreement histograms (no labels to avoid cardinality):
    // feature primary, gateway agrees after X ms
    metricVADAgreeGatewayMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "orch_vad_agree_gateway_ms",
        Help:    "When feature VAD is primary, time until gateway VAD agrees (ms)",
        Buckets: prometheus.ExponentialBuckets(5, 1.6, 12),
    })
    // gateway primary, feature agrees after X ms
    metricVADAgreeFeatureMS = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "orch_vad_agree_feature_ms",
        Help:    "When gateway VAD is primary, time until feature VAD agrees (ms)",
        Buckets: prometheus.ExponentialBuckets(5, 1.6, 12),
    })
)
