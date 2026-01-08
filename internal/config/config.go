package config

import (
    "fmt"
    "log"
    "strings"

    "github.com/spf13/viper"
)

type Config struct {
    Server struct {
        Port     string
        LogLevel string
    }
    Daily struct {
        APIKey          string
        Domain          string
        RoomPrefix      string
        RoomPrivacy     string
        BotName         string
        BotTokenExpMin  int
    }
    Bot struct {
        WorkerCmd            string
        StayConnectedSeconds string
    }
    Eleven struct {
        APIKey       string
        VoiceID      string
        CannedPhrase string
    }
}

func Load() Config {
    v := viper.New()
    v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
    v.AutomaticEnv()

    // Defaults
    v.SetDefault("server.port", 8080)
    v.SetDefault("server.log_level", "info")

    v.SetDefault("daily.room_prefix", "ai-interview-")
    v.SetDefault("daily.room_privacy", "private")
    v.SetDefault("daily.bot_name", "AI Interviewer")
    v.SetDefault("daily.bot_token_exp_min", 720)

    v.SetDefault("bot.stay_connected_seconds", 30)

    v.SetDefault("elevenlabs.canned_phrase", "Hi, I'm your AI interviewer. Can you hear me clearly?")

    // Map envs
    v.BindEnv("server.port", "PORT")
    v.BindEnv("server.log_level", "LOG_LEVEL")

    v.BindEnv("daily.api_key", "DAILY_API_KEY")
    v.BindEnv("daily.domain", "DAILY_DOMAIN")
    v.BindEnv("daily.room_prefix", "DAILY_ROOM_PREFIX")
    v.BindEnv("daily.room_privacy", "DAILY_ROOM_PRIVACY")
    v.BindEnv("daily.bot_name", "DAILY_BOT_NAME")
    v.BindEnv("daily.bot_token_exp_min", "DAILY_BOT_TOKEN_EXP_MIN")

    v.BindEnv("bot.worker_cmd", "BOT_WORKER_CMD")
    v.BindEnv("bot.stay_connected_seconds", "BOT_STAY_CONNECTED_SECONDS")

    v.BindEnv("elevenlabs.api_key", "ELEVENLABS_API_KEY")
    v.BindEnv("elevenlabs.voice_id", "ELEVENLABS_VOICE_ID")
    v.BindEnv("elevenlabs.canned_phrase", "ELEVENLABS_CANNED_PHRASE")

    var c Config
    c.Server.Port = toString(v.Get("server.port"))
    c.Server.LogLevel = v.GetString("server.log_level")

    c.Daily.APIKey = v.GetString("daily.api_key")
    c.Daily.Domain = v.GetString("daily.domain")
    c.Daily.RoomPrefix = v.GetString("daily.room_prefix")
    c.Daily.RoomPrivacy = v.GetString("daily.room_privacy")
    c.Daily.BotName = v.GetString("daily.bot_name")
    c.Daily.BotTokenExpMin = v.GetInt("daily.bot_token_exp_min")

    c.Bot.WorkerCmd = v.GetString("bot.worker_cmd")
    c.Bot.StayConnectedSeconds = toString(v.Get("bot.stay_connected_seconds"))

    c.Eleven.APIKey = v.GetString("elevenlabs.api_key")
    c.Eleven.VoiceID = v.GetString("elevenlabs.voice_id")
    c.Eleven.CannedPhrase = v.GetString("elevenlabs.canned_phrase")

    log.Printf("config loaded: port=%s daily_domain=%s", c.Server.Port, c.Daily.Domain)
    return c
}

func toString(v any) string { return fmt.Sprint(v) }
