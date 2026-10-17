// Package templates holds ready-made compose files for popular home lab apps,
// like Unraid's Community Applications. They're starting points: each opens
// in a settings form (built from its .env) and the editor before deploying.
package templates

import "strings"

// Template is one app starting point.
type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Port        string `json:"port,omitempty"`    // web UI port, shown on the card
	Project     string `json:"project,omitempty"` // the app's home page
	Notes       string `json:"notes,omitempty"`   // what to do after it starts
	Compose     string `json:"compose"`
	Env         string `json:"env"`
}

// d strips the leading newline so the YAML below can be indented naturally.
func d(s string) string { return strings.TrimPrefix(s, "\n") }

// v is one .env variable with the hint the install form shows for it.
func v(hint, key, value string) string { return "# " + hint + "\n" + key + "=" + value + "\n" }

// Shared .env lines. Apps run as the shares' owner, so files they write can
// be opened over Samba. {{…}} is filled in for this server (Values).
var (
	envTZ  = v("Time zone (a TZ database name)", "TZ", "{{TZ}}")
	envIDs = v("User ID the app runs as (the owner of the shares)", "PUID", "{{PUID}}") +
		v("Group ID the app runs as (the shares' group)", "PGID", "{{PGID}}")
	envBase = envTZ + envIDs
)

func port(hint, key, value string) string { return v(hint, key, value) }

// Values fill in what depends on the server: its time zone, the address
// apps are opened on, where the shares are and who owns them.
type Values struct {
	TZ     string // Europe/London
	Host   string // the address apps are opened on
	Shares string // /mnt/user
	PUID   string // the shares' owner
	PGID   string // and group
}

// With returns every template with v filled in.
func With(v Values) []Template {
	r := strings.NewReplacer("{{TZ}}", v.TZ, "{{HOST}}", v.Host, "{{SHARES}}", v.Shares, "{{PUID}}", v.PUID, "{{PGID}}", v.PGID)
	all := All()
	for i := range all {
		all[i].Env = r.Replace(all[i].Env)
		all[i].Notes = r.Replace(all[i].Notes)
	}
	return all
}

// All returns every template, in display order, with {{…}} for what
// depends on the server (see With).
func All() []Template {
	return []Template{
		{
			ID: "blank", Name: "Blank", Category: "Start",
			Description: "An empty compose file to write your own.",
			Compose: d(`
services:
  app:
    image: nginx:alpine
    ports:
      - "8080:80"
    restart: unless-stopped
`),
		},

		// ---------- Media ----------
		{
			ID: "jellyfin", Name: "Jellyfin", Category: "Media", Port: "8096", Project: "https://jellyfin.org",
			Description: "Free media server for your films, shows and music.",
			Compose: d(`
services:
  jellyfin:
    image: jellyfin/jellyfin:latest
    ports:
      - "${WEB_PORT}:8096"
    volumes:
      - ./config:/config
      - ./cache:/cache
      - ${MEDIA_PATH}:/media:ro
    environment:
      - TZ=${TZ}
    # Uncomment for Intel/AMD hardware transcoding:
    # devices:
    #   - /dev/dri:/dev/dri
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "8096") + v("Your films, shows and music (read-only to Jellyfin)", "MEDIA_PATH", "{{SHARES}}/Media"),
		},
		{
			ID: "plex", Name: "Plex", Category: "Media", Port: "32400", Project: "https://www.plex.tv",
			Description: "Popular media server with apps for every device.",
			Notes:       "Open http://<server>:32400/web to finish setup. A claim token links the server to your Plex account on first start.",
			Compose: d(`
services:
  plex:
    image: lscr.io/linuxserver/plex:latest
    network_mode: host
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - VERSION=docker
      - PLEX_CLAIM=${PLEX_CLAIM}
    volumes:
      - ./config:/config
      - ${MEDIA_PATH}:/media
    # Uncomment for hardware transcoding (Plex Pass):
    # devices:
    #   - /dev/dri:/dev/dri
    restart: unless-stopped
`),
			Env: envBase + v("Your films, shows and music", "MEDIA_PATH", "{{SHARES}}/Media") +
				v("Optional: a claim token from https://plex.tv/claim (valid for 4 minutes)", "PLEX_CLAIM", ""),
		},
		{
			ID: "emby", Name: "Emby", Category: "Media", Port: "8097", Project: "https://emby.media",
			Description: "Media server with polished apps and live TV.",
			Compose: d(`
services:
  emby:
    image: emby/embyserver:latest
    environment:
      - UID=${PUID}
      - GID=${PGID}
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:8096"
    volumes:
      - ./config:/config
      - ${MEDIA_PATH}:/media
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port (8097, so it can run next to Jellyfin)", "WEB_PORT", "8097") + v("Your films, shows and music", "MEDIA_PATH", "{{SHARES}}/Media"),
		},
		{
			ID: "navidrome", Name: "Navidrome", Category: "Media", Port: "4533", Project: "https://www.navidrome.org",
			Description: "Music streaming server that works with Subsonic apps.",
			Compose: d(`
services:
  navidrome:
    image: deluan/navidrome:latest
    ports:
      - "${WEB_PORT}:4533"
    environment:
      - ND_SCANSCHEDULE=1h
      - ND_LOGLEVEL=info
    volumes:
      - ./data:/data
      - ${MUSIC_PATH}:/music:ro
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "4533") + v("Your music library (read-only to Navidrome)", "MUSIC_PATH", "{{SHARES}}/Music"),
		},
		{
			ID: "jellyseerr", Name: "Jellyseerr", Category: "Media", Port: "5055", Project: "https://github.com/fallenbagel/jellyseerr",
			Description: "Lets your household request films and shows, for Jellyfin, Plex and Emby.",
			Compose: d(`
services:
  jellyseerr:
    image: fallenbagel/jellyseerr:latest
    environment:
      - TZ=${TZ}
      - LOG_LEVEL=info
    ports:
      - "${WEB_PORT}:5055"
    volumes:
      - ./config:/app/config
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "5055"),
		},
		{
			ID: "tautulli", Name: "Tautulli", Category: "Media", Port: "8181", Project: "https://tautulli.com",
			Description: "Watch statistics and notifications for Plex.",
			Compose: d(`
services:
  tautulli:
    image: lscr.io/linuxserver/tautulli:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
    ports:
      - "${WEB_PORT}:8181"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8181"),
		},

		// ---------- Media management ----------
		{
			ID: "arr", Name: "Sonarr and Radarr", Category: "Media management", Port: "8989", Project: "https://wiki.servarr.com",
			Description: "Automates TV shows and films, with Prowlarr for indexers.",
			Notes:       "Point Sonarr and Radarr at Prowlarr (Settings → Apps in Prowlarr), then at your download client. Use paths under /data in all of them.",
			Compose: d(`
services:
  prowlarr:
    image: lscr.io/linuxserver/prowlarr:latest
    environment: &env
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./prowlarr:/config
    ports:
      - "9696:9696"
    restart: unless-stopped

  sonarr:
    image: lscr.io/linuxserver/sonarr:latest
    environment: *env
    volumes:
      - ./sonarr:/config
      - ${DATA_PATH}:/data
    ports:
      - "8989:8989"
    restart: unless-stopped

  radarr:
    image: lscr.io/linuxserver/radarr:latest
    environment: *env
    volumes:
      - ./radarr:/config
      - ${DATA_PATH}:/data
    ports:
      - "7878:7878"
    restart: unless-stopped
`),
			Env: envBase + v("The folder holding both downloads and media, so imports are instant moves", "DATA_PATH", "{{SHARES}}"),
		},
		{
			ID: "lidarr", Name: "Lidarr", Category: "Media management", Port: "8686", Project: "https://lidarr.audio",
			Description: "Finds, downloads and organises music, like Sonarr for albums.",
			Compose: d(`
services:
  lidarr:
    image: lscr.io/linuxserver/lidarr:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - ${DATA_PATH}:/data
    ports:
      - "${WEB_PORT}:8686"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8686") + v("The folder holding both downloads and music", "DATA_PATH", "{{SHARES}}"),
		},
		{
			ID: "bazarr", Name: "Bazarr", Category: "Media management", Port: "6767", Project: "https://www.bazarr.media",
			Description: "Downloads subtitles for everything in Sonarr and Radarr.",
			Compose: d(`
services:
  bazarr:
    image: lscr.io/linuxserver/bazarr:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - ${DATA_PATH}:/data
    ports:
      - "${WEB_PORT}:6767"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "6767") + v("The same data folder Sonarr and Radarr use", "DATA_PATH", "{{SHARES}}"),
		},

		// ---------- Downloads ----------
		{
			ID: "qbittorrent", Name: "qBittorrent", Category: "Downloads", Port: "8090", Project: "https://www.qbittorrent.org",
			Description: "Torrent client with a web interface.",
			Notes:       "The first password for the admin user is printed in the container's log.",
			Compose: d(`
services:
  qbittorrent:
    image: lscr.io/linuxserver/qbittorrent:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - WEBUI_PORT=${WEB_PORT}
    volumes:
      - ./config:/config
      - ${DOWNLOADS_PATH}:/downloads
    ports:
      - "${WEB_PORT}:${WEB_PORT}"
      - "${TORRENT_PORT}:${TORRENT_PORT}"
      - "${TORRENT_PORT}:${TORRENT_PORT}/udp"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8090") + port("Port other peers connect to (forward it on your router for faster downloads)", "TORRENT_PORT", "6881") +
				v("Where downloads go", "DOWNLOADS_PATH", "{{SHARES}}/downloads"),
		},
		{
			ID: "transmission", Name: "Transmission", Category: "Downloads", Port: "9091", Project: "https://transmissionbt.com",
			Description: "Lightweight torrent client.",
			Compose: d(`
services:
  transmission:
    image: lscr.io/linuxserver/transmission:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - USER=${WEB_USER}
      - PASS=${WEB_PASSWORD}
    volumes:
      - ./config:/config
      - ./watch:/watch
      - ${DOWNLOADS_PATH}:/downloads
    ports:
      - "${WEB_PORT}:9091"
      - "51413:51413"
      - "51413:51413/udp"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "9091") + v("Web UI user name", "WEB_USER", "admin") + v("Web UI password", "WEB_PASSWORD", "change-me") +
				v("Where downloads go", "DOWNLOADS_PATH", "{{SHARES}}/downloads"),
		},
		{
			ID: "sabnzbd", Name: "SABnzbd", Category: "Downloads", Port: "8085", Project: "https://sabnzbd.org",
			Description: "Usenet downloader that's easy to set up.",
			Compose: d(`
services:
  sabnzbd:
    image: lscr.io/linuxserver/sabnzbd:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - ${DOWNLOADS_PATH}:/downloads
    ports:
      - "${WEB_PORT}:8080"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8085") + v("Where downloads go", "DOWNLOADS_PATH", "{{SHARES}}/downloads"),
		},
		{
			ID: "nzbget", Name: "NZBGet", Category: "Downloads", Port: "6789", Project: "https://nzbget.com",
			Description: "Fast, efficient Usenet downloader.",
			Compose: d(`
services:
  nzbget:
    image: lscr.io/linuxserver/nzbget:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - NZBGET_USER=${WEB_USER}
      - NZBGET_PASS=${WEB_PASSWORD}
    volumes:
      - ./config:/config
      - ${DOWNLOADS_PATH}:/downloads
    ports:
      - "${WEB_PORT}:6789"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "6789") + v("Web UI user name", "WEB_USER", "nzbget") + v("Web UI password", "WEB_PASSWORD", "change-me") +
				v("Where downloads go", "DOWNLOADS_PATH", "{{SHARES}}/downloads"),
		},
		{
			ID: "metube", Name: "MeTube", Category: "Downloads", Port: "8086", Project: "https://github.com/alexta69/metube",
			Description: "Download videos from YouTube and hundreds of other sites (yt-dlp).",
			Compose: d(`
services:
  metube:
    image: ghcr.io/alexta69/metube:latest
    environment:
      - UID=${PUID}
      - GID=${PGID}
    ports:
      - "${WEB_PORT}:8081"
    volumes:
      - ${DOWNLOADS_PATH}:/downloads
    restart: unless-stopped
`),
			Env: envIDs + port("Web UI port", "WEB_PORT", "8086") + v("Where videos are saved", "DOWNLOADS_PATH", "{{SHARES}}/downloads/metube"),
		},
		{
			ID: "jdownloader", Name: "JDownloader 2", Category: "Downloads", Port: "5802", Project: "https://jdownloader.org",
			Description: "Download manager for file hosts, in your browser.",
			Compose: d(`
services:
  jdownloader:
    image: jlesage/jdownloader-2:latest
    environment:
      - USER_ID=${PUID}
      - GROUP_ID=${PGID}
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:5800"
    volumes:
      - ./config:/config
      - ${DOWNLOADS_PATH}:/output
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "5802") + v("Where downloads go", "DOWNLOADS_PATH", "{{SHARES}}/downloads"),
		},

		// ---------- Photos and books ----------
		{
			ID: "immich", Name: "Immich", Category: "Photos and books", Port: "2283", Project: "https://immich.app",
			Description: "Self-hosted Google Photos: phone backup, faces, maps and search.",
			Notes:       "Before a major Immich update, check its release notes: the database image sometimes changes. Install the phone app and sign in with the server's address.",
			Compose: d(`
services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION}
    volumes:
      - ${UPLOAD_LOCATION}:/data
      - /etc/localtime:/etc/localtime:ro
    env_file:
      - .env
    ports:
      - "${WEB_PORT}:2283"
    depends_on:
      - redis
      - database
    restart: unless-stopped

  immich-machine-learning:
    image: ghcr.io/immich-app/immich-machine-learning:${IMMICH_VERSION}
    volumes:
      - model-cache:/cache
    env_file:
      - .env
    restart: unless-stopped

  redis:
    image: docker.io/valkey/valkey:8-bookworm
    restart: unless-stopped

  database:
    image: ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_USER: ${DB_USERNAME}
      POSTGRES_DB: ${DB_DATABASE_NAME}
      POSTGRES_INITDB_ARGS: "--data-checksums"
    volumes:
      - ${DB_DATA_LOCATION}:/var/lib/postgresql/data
    shm_size: 128mb
    restart: unless-stopped

volumes:
  model-cache:
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "2283") + v("Where photos and videos are stored (a share such as {{SHARES}}/Photos keeps them on the array)", "UPLOAD_LOCATION", "./library") +
				v("The database (keep it on the cache, not the array)", "DB_DATA_LOCATION", "./postgres") +
				v("Immich version: release follows the latest", "IMMICH_VERSION", "release") +
				v("Database password (only used inside this stack)", "DB_PASSWORD", "change-me") +
				v("Database user", "DB_USERNAME", "postgres") + v("Database name", "DB_DATABASE_NAME", "immich"),
		},
		{
			ID: "audiobookshelf", Name: "Audiobookshelf", Category: "Photos and books", Port: "13378", Project: "https://www.audiobookshelf.org",
			Description: "Audiobook and podcast server with apps that remember your place.",
			Compose: d(`
services:
  audiobookshelf:
    image: ghcr.io/advplyr/audiobookshelf:latest
    ports:
      - "${WEB_PORT}:80"
    environment:
      - TZ=${TZ}
    volumes:
      - ${AUDIOBOOKS_PATH}:/audiobooks
      - ${PODCASTS_PATH}:/podcasts
      - ./config:/config
      - ./metadata:/metadata
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "13378") + v("Your audiobooks", "AUDIOBOOKS_PATH", "{{SHARES}}/Media/Audiobooks") +
				v("Where podcasts are kept", "PODCASTS_PATH", "{{SHARES}}/Media/Podcasts"),
		},
		{
			ID: "kavita", Name: "Kavita", Category: "Photos and books", Port: "5000", Project: "https://www.kavitareader.com",
			Description: "Reading server for comics, manga and ebooks.",
			Compose: d(`
services:
  kavita:
    image: jvmilazz0/kavita:latest
    environment:
      - TZ=${TZ}
    volumes:
      - ./config:/kavita/config
      - ${BOOKS_PATH}:/books
    ports:
      - "${WEB_PORT}:5000"
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "5000") + v("Your comics, manga and ebooks", "BOOKS_PATH", "{{SHARES}}/Media/Books"),
		},
		{
			ID: "calibre-web", Name: "Calibre-Web", Category: "Photos and books", Port: "8084", Project: "https://github.com/janeczku/calibre-web",
			Description: "Browse, read and download the books in a Calibre library.",
			Notes:       "Sign in as admin / admin123, change the password, then set the library folder to /books.",
			Compose: d(`
services:
  calibre-web:
    image: lscr.io/linuxserver/calibre-web:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - ${LIBRARY_PATH}:/books
    ports:
      - "${WEB_PORT}:8083"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8084") + v("Your Calibre library (the folder with metadata.db)", "LIBRARY_PATH", "{{SHARES}}/Media/Books/Calibre"),
		},

		// ---------- Media tools ----------
		{
			ID: "handbrake", Name: "HandBrake", Category: "Media tools", Port: "5800", Project: "https://handbrake.fr",
			Description: "Converts video to other formats, in your browser.",
			Compose: d(`
services:
  handbrake:
    image: jlesage/handbrake:latest
    environment:
      - USER_ID=${PUID}
      - GROUP_ID=${PGID}
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:5800"
    volumes:
      - ./config:/config
      - ${MEDIA_PATH}:/storage:ro
      - ./watch:/watch
      - ${OUTPUT_PATH}:/output
    # Uncomment for Intel Quick Sync encoding:
    # devices:
    #   - /dev/dri:/dev/dri
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "5800") + v("Videos to convert (read-only)", "MEDIA_PATH", "{{SHARES}}/Media") +
				v("Where converted videos go", "OUTPUT_PATH", "{{SHARES}}/downloads/handbrake"),
		},
		{
			ID: "makemkv", Name: "MakeMKV", Category: "Media tools", Port: "5801", Project: "https://www.makemkv.com",
			Description: "Rips DVDs and Blu-rays to MKV files, in your browser.",
			Compose: d(`
services:
  makemkv:
    image: jlesage/makemkv:latest
    environment:
      - USER_ID=${PUID}
      - GROUP_ID=${PGID}
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:5800"
    volumes:
      - ./config:/config
      - ${OUTPUT_PATH}:/output
    # The optical drive (ls -l /dev/sr* /dev/sg* on the server):
    # devices:
    #   - /dev/sr0:/dev/sr0
    #   - /dev/sg0:/dev/sg0
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "5801") + v("Where rips go", "OUTPUT_PATH", "{{SHARES}}/downloads/makemkv"),
		},

		// ---------- Home automation ----------
		{
			ID: "homeassistant", Name: "Home Assistant", Category: "Home automation", Port: "8123", Project: "https://www.home-assistant.io",
			Description: "Open-source home automation for all your smart devices.",
			Compose: d(`
services:
  homeassistant:
    image: ghcr.io/home-assistant/home-assistant:stable
    network_mode: host
    privileged: true
    environment:
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - /etc/localtime:/etc/localtime:ro
      - /run/dbus:/run/dbus:ro
    restart: unless-stopped
`),
			Env: envTZ,
		},
		{
			ID: "mosquitto", Name: "Mosquitto", Category: "Home automation", Port: "1883", Project: "https://mosquitto.org",
			Description: "MQTT broker for Zigbee2MQTT, ESPHome, Tasmota and Home Assistant.",
			Notes:       "This starts without passwords, for your LAN only. To add users, put a mosquitto.conf in ./config and change the command to use it.",
			Compose: d(`
services:
  mosquitto:
    image: eclipse-mosquitto:2
    command: mosquitto -c /mosquitto-no-auth.conf
    ports:
      - "${MQTT_PORT}:1883"
      - "${WS_PORT}:9001"
    volumes:
      - ./data:/mosquitto/data
      - ./log:/mosquitto/log
    restart: unless-stopped
`),
			Env: port("MQTT port", "MQTT_PORT", "1883") + port("MQTT over WebSockets port", "WS_PORT", "9001"),
		},
		{
			ID: "zigbee2mqtt", Name: "Zigbee2MQTT", Category: "Home automation", Port: "8088", Project: "https://www.zigbee2mqtt.io",
			Description: "Use Zigbee devices without the vendors' hubs, through MQTT.",
			Notes:       "Needs an MQTT broker (the Mosquitto template) and a Zigbee adapter. The first start opens a setup page on the web UI port.",
			Compose: d(`
services:
  zigbee2mqtt:
    image: koenkk/zigbee2mqtt:latest
    environment:
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:8080"
    volumes:
      - ./data:/app/data
      - /run/udev:/run/udev:ro
    devices:
      - ${ZIGBEE_ADAPTER}:/dev/ttyACM0
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "8088") +
				v("Your Zigbee adapter (ls /dev/serial/by-id/ on the server)", "ZIGBEE_ADAPTER", "/dev/serial/by-id/usb-your-adapter"),
		},
		{
			ID: "node-red", Name: "Node-RED", Category: "Home automation", Port: "1880", Project: "https://nodered.org",
			Description: "Wire up automations and APIs with a visual flow editor.",
			Compose: d(`
services:
  node-red:
    image: nodered/node-red:latest
    environment:
      - TZ=${TZ}
    ports:
      - "${WEB_PORT}:1880"
    volumes:
      - node-red-data:/data
    restart: unless-stopped

volumes:
  node-red-data:
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "1880"),
		},
		{
			ID: "esphome", Name: "ESPHome", Category: "Home automation", Port: "6052", Project: "https://esphome.io",
			Description: "Build and update firmware for ESP32 and ESP8266 devices.",
			Compose: d(`
services:
  esphome:
    image: ghcr.io/esphome/esphome:stable
    network_mode: host
    environment:
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped
`),
			Env: envTZ,
		},

		// ---------- Network ----------
		{
			ID: "pihole", Name: "Pi-hole", Category: "Network", Port: "8080", Project: "https://pi-hole.net",
			Description: "Blocks ads and trackers for every device on your network.",
			Notes:       "The admin page is at /admin. Point your router's DNS at this server.",
			Compose: d(`
services:
  pihole:
    image: pihole/pihole:latest
    ports:
      - "53:53/tcp"
      - "53:53/udp"
      - "${WEB_PORT}:80/tcp"
    environment:
      - TZ=${TZ}
      - FTLCONF_webserver_api_password=${WEB_PASSWORD}
      - FTLCONF_dns_listeningMode=all
    volumes:
      - ./etc-pihole:/etc/pihole
    cap_add:
      - SYS_NICE
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "8080") + v("Admin page password", "WEB_PASSWORD", "change-me"),
		},
		{
			ID: "adguard", Name: "AdGuard Home", Category: "Network", Port: "3000", Project: "https://adguard.com/adguard-home/overview.html",
			Description: "Network-wide ad blocking and encrypted DNS.",
			Compose: d(`
services:
  adguardhome:
    image: adguard/adguardhome:latest
    ports:
      - "53:53/tcp"
      - "53:53/udp"
      - "${SETUP_PORT}:3000/tcp" # first-run setup
      - "${WEB_PORT}:80/tcp"     # dashboard after setup
    volumes:
      - ./work:/opt/adguardhome/work
      - ./conf:/opt/adguardhome/conf
    restart: unless-stopped
`),
			Env: port("First-run setup port", "SETUP_PORT", "3000") + port("Dashboard port (after setup)", "WEB_PORT", "8081"),
		},
		{
			ID: "npm", Name: "Nginx Proxy Manager", Category: "Network", Port: "81", Project: "https://nginxproxymanager.com",
			Description: "Reverse proxy with free HTTPS certificates and a simple UI.",
			Notes:       "Takes ports 80 and 443. The admin UI on port 81 asks you to create the first user.",
			Compose: d(`
services:
  npm:
    image: jc21/nginx-proxy-manager:latest
    ports:
      - "80:80"
      - "443:443"
      - "${WEB_PORT}:81"
    volumes:
      - ./data:/data
      - ./letsencrypt:/etc/letsencrypt
    restart: unless-stopped
`),
			Env: port("Admin UI port", "WEB_PORT", "81"),
		},
		{
			ID: "wg-easy", Name: "WireGuard Easy", Category: "Network", Port: "51821", Project: "https://wg-easy.github.io/wg-easy/",
			Description: "WireGuard VPN with a web UI for adding phones and laptops.",
			Notes:       "Forward the VPN port (UDP) on your router to this server. The web UI asks for an admin account on first visit.",
			Compose: d(`
services:
  wg-easy:
    image: ghcr.io/wg-easy/wg-easy:15
    environment:
      # The web UI is plain HTTP on your LAN.
      - INSECURE=true
    volumes:
      - ./config:/etc/wireguard
      - /lib/modules:/lib/modules:ro
    ports:
      - "${VPN_PORT}:51820/udp"
      - "${WEB_PORT}:51821/tcp"
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    sysctls:
      - net.ipv4.ip_forward=1
      - net.ipv4.conf.all.src_valid_mark=1
      - net.ipv6.conf.all.disable_ipv6=0
      - net.ipv6.conf.all.forwarding=1
      - net.ipv6.conf.default.forwarding=1
    restart: unless-stopped
`),
			Env: port("VPN port (UDP): forward this on your router", "VPN_PORT", "51820") + port("Web UI port", "WEB_PORT", "51821"),
		},
		{
			ID: "tailscale", Name: "Tailscale", Category: "Network", Project: "https://tailscale.com",
			Description: "Reach this server from anywhere over your Tailscale network.",
			Notes:       "Create an auth key at https://login.tailscale.com/admin/settings/keys. Homestack already allows the 100.64.x range Tailscale uses.",
			Compose: d(`
services:
  tailscale:
    image: tailscale/tailscale:latest
    hostname: ${TS_HOSTNAME}
    network_mode: host
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}
      - TS_STATE_DIR=/var/lib/tailscale
      - TS_USERSPACE=false
      # - TS_EXTRA_ARGS=--advertise-exit-node --advertise-routes=192.168.1.0/24
    volumes:
      - ./state:/var/lib/tailscale
      - /dev/net/tun:/dev/net/tun
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    restart: unless-stopped
`),
			Env: v("Auth key from the Tailscale admin console", "TS_AUTHKEY", "") + v("The name this server gets on your tailnet", "TS_HOSTNAME", "homestack"),
		},
		{
			ID: "cloudflared", Name: "Cloudflare Tunnel", Category: "Network", Project: "https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/",
			Description: "Publish chosen apps through Cloudflare without opening ports.",
			Notes:       "Create a tunnel in the Cloudflare Zero Trust dashboard and copy its token. Keep Homestack itself off the tunnel.",
			Compose: d(`
services:
  cloudflared:
    image: cloudflare/cloudflared:latest
    command: tunnel --no-autoupdate run
    environment:
      - TUNNEL_TOKEN=${TUNNEL_TOKEN}
    restart: unless-stopped
`),
			Env: v("The tunnel's token from the Cloudflare dashboard", "TUNNEL_TOKEN", ""),
		},

		// ---------- Monitoring ----------
		{
			ID: "uptimekuma", Name: "Uptime Kuma", Category: "Monitoring", Port: "3001", Project: "https://uptime.kuma.pet",
			Description: "Monitors your services and alerts you when one goes down.",
			Compose: d(`
services:
  uptime-kuma:
    image: louislam/uptime-kuma:2
    ports:
      - "${WEB_PORT}:3001"
    volumes:
      - ./data:/app/data
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "3001"),
		},
		{
			ID: "homepage", Name: "Homepage", Category: "Monitoring", Port: "3002", Project: "https://gethomepage.dev",
			Description: "A start page for your home lab with live service widgets.",
			Compose: d(`
services:
  homepage:
    image: ghcr.io/gethomepage/homepage:latest
    ports:
      - "${WEB_PORT}:3000"
    environment:
      - HOMEPAGE_ALLOWED_HOSTS=${ALLOWED_HOSTS}
      - PUID=${PUID}
      - PGID=${PGID}
    volumes:
      - ./config:/app/config
      # Optional: lets Homepage show container status
      # - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "3002") + v("The address and port you'll open Homepage on", "ALLOWED_HOSTS", "{{HOST}}:3002"),
		},
		{
			ID: "netdata", Name: "Netdata", Category: "Monitoring", Port: "19999", Project: "https://www.netdata.cloud",
			Description: "Detailed real-time charts for every part of the server.",
			Compose: d(`
services:
  netdata:
    image: netdata/netdata:stable
    pid: host
    network_mode: host
    cap_add:
      - SYS_PTRACE
      - SYS_ADMIN
    security_opt:
      - apparmor:unconfined
    volumes:
      - netdataconfig:/etc/netdata
      - netdatalib:/var/lib/netdata
      - netdatacache:/var/cache/netdata
      - /:/host/root:ro,rslave
      - /etc/passwd:/host/etc/passwd:ro
      - /etc/group:/host/etc/group:ro
      - /etc/localtime:/etc/localtime:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /etc/os-release:/host/etc/os-release:ro
      - /var/log:/host/var/log:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped

volumes:
  netdataconfig:
  netdatalib:
  netdatacache:
`),
		},
		{
			ID: "grafana", Name: "Grafana", Category: "Monitoring", Port: "3003", Project: "https://grafana.com",
			Description: "Dashboards and graphs for Prometheus, InfluxDB and more.",
			Notes:       "Sign in as admin with the password below.",
			Compose: d(`
services:
  grafana:
    image: grafana/grafana:latest
    ports:
      - "${WEB_PORT}:3000"
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=${ADMIN_PASSWORD}
    volumes:
      - grafana-data:/var/lib/grafana
    restart: unless-stopped

volumes:
  grafana-data:
`),
			Env: port("Web UI port", "WEB_PORT", "3003") + v("Password for the admin user", "ADMIN_PASSWORD", "change-me"),
		},
		{
			ID: "dozzle", Name: "Dozzle", Category: "Monitoring", Port: "8888", Project: "https://dozzle.dev",
			Description: "Live logs from all your containers in one place.",
			Compose: d(`
services:
  dozzle:
    image: amir20/dozzle:latest
    ports:
      - "${WEB_PORT}:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "8888"),
		},
		{
			ID: "changedetection", Name: "changedetection.io", Category: "Monitoring", Port: "5002", Project: "https://changedetection.io",
			Description: "Watches web pages and tells you when they change (prices, stock, news).",
			Compose: d(`
services:
  changedetection:
    image: ghcr.io/dgtlmoon/changedetection.io:latest
    ports:
      - "${WEB_PORT}:5000"
    volumes:
      - ./datastore:/datastore
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "5002"),
		},
		{
			ID: "ntfy", Name: "ntfy", Category: "Monitoring", Port: "8092", Project: "https://ntfy.sh",
			Description: "Your own push notification server for phones and scripts.",
			Notes:       "Homestack can send its alerts here: Settings → Notifications → ntfy.",
			Compose: d(`
services:
  ntfy:
    image: binwiederhier/ntfy:latest
    command: serve
    environment:
      - TZ=${TZ}
      - NTFY_BASE_URL=${BASE_URL}
      - NTFY_CACHE_FILE=/var/cache/ntfy/cache.db
      - NTFY_ATTACHMENT_CACHE_DIR=/var/cache/ntfy/attachments
    ports:
      - "${WEB_PORT}:80"
    volumes:
      - ./cache:/var/cache/ntfy
      - ./config:/etc/ntfy
    restart: unless-stopped
`),
			Env: envTZ + port("Web UI port", "WEB_PORT", "8092") + v("The address phones will use to reach it", "BASE_URL", "http://{{HOST}}:8092"),
		},

		// ---------- Productivity ----------
		{
			ID: "paperless", Name: "Paperless-ngx", Category: "Productivity", Port: "8000", Project: "https://docs.paperless-ngx.com",
			Description: "Scan, index and search all your paper documents.",
			Notes:       "Create the first user with: docker exec -it <stack>-webserver-1 createsuperuser",
			Compose: d(`
services:
  broker:
    image: docker.io/library/redis:7
    volumes:
      - ./redis:/data
    restart: unless-stopped

  webserver:
    image: ghcr.io/paperless-ngx/paperless-ngx:latest
    depends_on:
      - broker
    ports:
      - "${WEB_PORT}:8000"
    environment:
      - PAPERLESS_REDIS=redis://broker:6379
      - PAPERLESS_TIME_ZONE=${TZ}
      - USERMAP_UID=${PUID}
      - USERMAP_GID=${PGID}
    volumes:
      - ./data:/usr/src/paperless/data
      - ./media:/usr/src/paperless/media
      - ./export:/usr/src/paperless/export
      - ${CONSUME_PATH}:/usr/src/paperless/consume
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8000") + v("Drop scans here to import them (a share works well)", "CONSUME_PATH", "./consume"),
		},
		{
			ID: "nextcloud", Name: "Nextcloud", Category: "Productivity", Port: "8083", Project: "https://nextcloud.com",
			Description: "Your own cloud for files, calendars and contacts.",
			Compose: d(`
services:
  db:
    image: mariadb:11
    environment:
      - MARIADB_ROOT_PASSWORD=${DB_ROOT_PASSWORD}
      - MARIADB_DATABASE=nextcloud
      - MARIADB_USER=nextcloud
      - MARIADB_PASSWORD=${DB_PASSWORD}
    volumes:
      - ./db:/var/lib/mysql
    restart: unless-stopped

  app:
    image: nextcloud:latest
    depends_on:
      - db
    ports:
      - "${WEB_PORT}:80"
    environment:
      - MYSQL_HOST=db
      - MYSQL_DATABASE=nextcloud
      - MYSQL_USER=nextcloud
      - MYSQL_PASSWORD=${DB_PASSWORD}
    volumes:
      - ./html:/var/www/html
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "8083") + v("Database root password (only used inside this stack)", "DB_ROOT_PASSWORD", "change-me") +
				v("Database password for Nextcloud", "DB_PASSWORD", "change-me"),
		},
		{
			ID: "mealie", Name: "Mealie", Category: "Productivity", Port: "9925", Project: "https://mealie.io",
			Description: "Recipe manager and meal planner that imports recipes from any site.",
			Notes:       "Sign in with changeme@example.com / MyPassword and change it straight away.",
			Compose: d(`
services:
  mealie:
    image: ghcr.io/mealie-recipes/mealie:latest
    ports:
      - "${WEB_PORT}:9000"
    environment:
      - ALLOW_SIGNUP=false
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - BASE_URL=${BASE_URL}
    volumes:
      - ./data:/app/data
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "9925") + v("The address you'll open Mealie on", "BASE_URL", "http://{{HOST}}:9925"),
		},
		{
			ID: "stirling-pdf", Name: "Stirling PDF", Category: "Productivity", Port: "8087", Project: "https://www.stirlingpdf.com",
			Description: "Merge, split, convert, OCR and sign PDFs, all on your server.",
			Compose: d(`
services:
  stirling-pdf:
    image: stirlingtools/stirling-pdf:latest
    ports:
      - "${WEB_PORT}:8080"
    environment:
      - DOCKER_ENABLE_SECURITY=false
    volumes:
      - ./tessdata:/usr/share/tessdata
      - ./configs:/configs
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "8087"),
		},
		{
			ID: "freshrss", Name: "FreshRSS", Category: "Productivity", Port: "8089", Project: "https://freshrss.org",
			Description: "RSS reader that syncs with mobile apps.",
			Compose: d(`
services:
  freshrss:
    image: lscr.io/linuxserver/freshrss:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
    ports:
      - "${WEB_PORT}:80"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8089"),
		},
		{
			ID: "linkding", Name: "linkding", Category: "Productivity", Port: "9090", Project: "https://linkding.link",
			Description: "Minimal bookmark manager with tags and browser extensions.",
			Compose: d(`
services:
  linkding:
    image: sissbruecker/linkding:latest
    ports:
      - "${WEB_PORT}:9090"
    environment:
      - LD_SUPERUSER_NAME=${ADMIN_USER}
      - LD_SUPERUSER_PASSWORD=${ADMIN_PASSWORD}
    volumes:
      - ./data:/etc/linkding/data
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "9090") + v("Admin user name", "ADMIN_USER", "admin") + v("Admin password", "ADMIN_PASSWORD", "change-me"),
		},
		{
			ID: "actual", Name: "Actual Budget", Category: "Productivity", Port: "5006", Project: "https://actualbudget.org",
			Description: "Private envelope budgeting that syncs between devices.",
			Compose: d(`
services:
  actual:
    image: actualbudget/actual-server:latest
    ports:
      - "${WEB_PORT}:5006"
    volumes:
      - ./data:/data
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "5006"),
		},

		// ---------- Security ----------
		{
			ID: "vaultwarden", Name: "Vaultwarden", Category: "Security", Port: "8082", Project: "https://github.com/dani-garcia/vaultwarden",
			Description: "Lightweight password manager that works with Bitwarden apps.",
			Notes:       "Browsers only allow the web vault over HTTPS: put it behind a reverse proxy, and turn sign-ups off once your accounts exist.",
			Compose: d(`
services:
  vaultwarden:
    image: vaultwarden/server:latest
    ports:
      - "${WEB_PORT}:80"
    environment:
      - DOMAIN=${DOMAIN}
      - SIGNUPS_ALLOWED=${SIGNUPS_ALLOWED}
    volumes:
      - ./data:/data
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "8082") + v("Your vault's HTTPS address (behind a reverse proxy)", "DOMAIN", "https://vault.home.lan") +
				v("Allow new accounts (set to false once yours exist)", "SIGNUPS_ALLOWED", "true"),
		},

		// ---------- Files and backup ----------
		{
			ID: "syncthing", Name: "Syncthing", Category: "Files and backup", Port: "8384", Project: "https://syncthing.net",
			Description: "Keeps folders in sync between your devices.",
			Compose: d(`
services:
  syncthing:
    image: lscr.io/linuxserver/syncthing:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
    volumes:
      - ./config:/config
      - ${SYNC_PATH}:/data
    ports:
      - "${WEB_PORT}:8384"
      - "22000:22000/tcp"
      - "22000:22000/udp"
      - "21027:21027/udp"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8384") + v("The folder to sync (a share path, or ./sync inside this stack)", "SYNC_PATH", "./sync"),
		},
		{
			ID: "duplicati", Name: "Duplicati", Category: "Files and backup", Port: "8200", Project: "https://duplicati.com",
			Description: "Encrypted, scheduled backups to the cloud, another server or a USB disk.",
			Notes:       "Keep a copy of the settings encryption key somewhere safe: Duplicati can't read its own settings without it.",
			Compose: d(`
services:
  duplicati:
    image: lscr.io/linuxserver/duplicati:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - SETTINGS_ENCRYPTION_KEY=${SETTINGS_KEY}
      - DUPLICATI__WEBSERVICE_PASSWORD=${WEB_PASSWORD}
    volumes:
      - ./config:/config
      - ${BACKUPS_PATH}:/backups
      - ${SOURCE_PATH}:/source:ro
    ports:
      - "${WEB_PORT}:8200"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8200") + v("Web UI password", "WEB_PASSWORD", "change-me") +
				v("Encrypts Duplicati's own settings (keep a copy)", "SETTINGS_KEY", "change-me") +
				v("What to back up (read-only)", "SOURCE_PATH", "{{SHARES}}") + v("A local folder to back up to (optional)", "BACKUPS_PATH", "./backups"),
		},

		// ---------- Development ----------
		{
			ID: "gitea", Name: "Gitea", Category: "Development", Port: "3000", Project: "https://about.gitea.com",
			Description: "Lightweight self-hosted Git service.",
			Compose: d(`
services:
  gitea:
    image: gitea/gitea:1
    environment:
      - USER_UID=${PUID}
      - USER_GID=${PGID}
    volumes:
      - ./data:/data
      - /etc/localtime:/etc/localtime:ro
    ports:
      - "${WEB_PORT}:3000"
      - "${SSH_PORT}:22"
    restart: unless-stopped
`),
			Env: v("User ID Gitea runs as", "PUID", "1000") + v("Group ID Gitea runs as", "PGID", "1000") +
				port("Web UI port", "WEB_PORT", "3000") + port("SSH port for git clone over SSH", "SSH_PORT", "2222"),
		},
		{
			ID: "code-server", Name: "code-server", Category: "Development", Port: "8443", Project: "https://coder.com/docs/code-server",
			Description: "VS Code in your browser, running on the server.",
			Compose: d(`
services:
  code-server:
    image: lscr.io/linuxserver/code-server:latest
    environment:
      - PUID=${PUID}
      - PGID=${PGID}
      - TZ=${TZ}
      - PASSWORD=${WEB_PASSWORD}
      - DEFAULT_WORKSPACE=/workspace
    volumes:
      - ./config:/config
      - ${WORKSPACE_PATH}:/workspace
    ports:
      - "${WEB_PORT}:8443"
    restart: unless-stopped
`),
			Env: envBase + port("Web UI port", "WEB_PORT", "8443") + v("Password for the web UI", "WEB_PASSWORD", "change-me") +
				v("The folder to open (your projects)", "WORKSPACE_PATH", "./workspace"),
		},

		// ---------- AI ----------
		{
			ID: "open-webui", Name: "Open WebUI with Ollama", Category: "AI", Port: "3004", Project: "https://openwebui.com",
			Description: "Chat with AI models that run entirely on your server.",
			Notes:       "Create the admin account on first visit, then download a model under Settings → Models. Models need several GB of memory.",
			Compose: d(`
services:
  open-webui:
    image: ghcr.io/open-webui/open-webui:ollama
    ports:
      - "${WEB_PORT}:8080"
    volumes:
      - ./ollama:/root/.ollama
      - ./open-webui:/app/backend/data
    # With an NVIDIA card and the NVIDIA container toolkit:
    # deploy:
    #   resources:
    #     reservations:
    #       devices:
    #         - driver: nvidia
    #           count: all
    #           capabilities: [gpu]
    restart: unless-stopped
`),
			Env: port("Web UI port", "WEB_PORT", "3004"),
		},

		// ---------- Games ----------
		{
			ID: "minecraft", Name: "Minecraft server", Category: "Games", Port: "25565", Project: "https://docker-minecraft-server.readthedocs.io",
			Description: "Java Edition server (Vanilla, Paper, Fabric, Forge…).",
			Notes:       "Set EULA to TRUE to accept Minecraft's licence; the server won't start otherwise.",
			Compose: d(`
services:
  minecraft:
    image: itzg/minecraft-server:latest
    tty: true
    stdin_open: true
    ports:
      - "${GAME_PORT}:25565"
    environment:
      - EULA=${EULA}
      - TYPE=${TYPE}
      - MEMORY=${MEMORY}
      - TZ=${TZ}
    volumes:
      - ./data:/data
    restart: unless-stopped
`),
			Env: envTZ + port("Port players connect to", "GAME_PORT", "25565") + v("TRUE accepts the Minecraft EULA (https://aka.ms/MinecraftEULA)", "EULA", "FALSE") +
				v("Server type: VANILLA, PAPER, FABRIC, FORGE…", "TYPE", "PAPER") + v("Memory for the server", "MEMORY", "4G"),
		},
	}
}
