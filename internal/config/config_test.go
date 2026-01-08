package config

import (
    "os"
    "testing"
)

func TestLoadDefaults(t *testing.T) {
    // Clear relevant envs
    os.Unsetenv("PORT")
    os.Unsetenv("LOG_LEVEL")
    os.Unsetenv("DAILY_ROOM_PREFIX")
    os.Unsetenv("DAILY_ROOM_PRIVACY")

    c := Load()

    if c.Server.Port != "8080" {
        t.Fatalf("expected default port 8080, got %q", c.Server.Port)
    }
    if c.Server.LogLevel != "info" {
        t.Fatalf("expected default log level info, got %q", c.Server.LogLevel)
    }
    if c.Daily.RoomPrefix != "ai-interview-" {
        t.Fatalf("expected default room prefix, got %q", c.Daily.RoomPrefix)
    }
    if c.Daily.RoomPrivacy != "private" {
        t.Fatalf("expected default room privacy private, got %q", c.Daily.RoomPrivacy)
    }
}

