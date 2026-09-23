// ============================================================
//  Dragon Land Discord Bot
//  npm install discord.js
// ============================================================

const {
  Client, GatewayIntentBits, EmbedBuilder,
  SlashCommandBuilder, REST, Routes, ActivityType, Partials,
  ActionRowBuilder, ButtonBuilder, ButtonStyle, ComponentType,
} = require("discord.js");
const fs   = require("fs");
const path = require("path");
const https = require("https");
const http  = require("http");

// Load /opt/dragonland-dlr/.env (parent of discord/)
(function loadLocalEnv() {
  try {
    const envPath = path.join(__dirname, "..", ".env");
    if (!fs.existsSync(envPath)) return;
    const txt = fs.readFileSync(envPath, "utf8");
    for (const line of txt.split(/\r?\n/)) {
      const s = line.trim();
      if (!s || s.startsWith("#")) continue;
      const eq = s.indexOf("=");
      if (eq < 1) continue;
      const k = s.slice(0, eq).trim();
      let v = s.slice(eq + 1).trim();
      if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'")))
        v = v.slice(1, -1);
      if (process.env[k] === undefined || process.env[k] === "") process.env[k] = v;
    }
  } catch (e) {
    console.warn(".env load warning:", e.message);
  }
})();

function envList(key, fallback) {
  const raw = (process.env[key] || "").trim();
  if (!raw) return fallback;
  return raw.split(",").map((s) => s.trim()).filter(Boolean);
}

// ── Config (2.5.1 public / dlrp — edit /opt/dragonland-dlr/.env) ─
const CONFIG = {
  TOKEN:     process.env.DISCORD_BOT_TOKEN,
  CLIENT_ID: (process.env.DISCORD_CLIENT_ID || "").trim(),
  GUILD_ID:  (process.env.DISCORD_GUILD_ID || "").trim(),

  API_BASE: (process.env.DRAGONLAND_API_BASE || "http://127.0.0.1:5193").replace(/\/$/, ""),
  API_PREFIX: "/RHJhZ29uTGFuZA/api/v3",
  BOT_USER_ID: 1,

  OWNER_ID: (process.env.DISCORD_OWNER_ID || "722100095524405329").trim(),
  ADMIN_IDS: envList("DISCORD_ADMIN_IDS", ["819847542413852692", "1224764351093674146"]),
  BOT_SECRET: process.env.BOT_SECRET,
  MEME_EMOJI_NAME: "roggy",
  MEME_EMOJI_THRESHOLD: 5,

  FT_RANKS: [
    { key: "bronze", label: "Bronze", minScore: 500,  emoji: "Bronzerank", color: 0xcd7f32 },
    { key: "silver", label: "Silver", minScore: 1000, emoji: "Silverrank", color: 0xc0c0c0 },
    { key: "gold",   label: "Gold",   minScore: 2000, emoji: "Goldenrank", color: 0xf1c40f },
  ],
  FT_RANK_ANNOUNCE_CHANNEL: (process.env.DISCORD_FT_RANK_CHANNEL_ID || "").trim(),
  FT_RANK_POLL_INTERVAL_MS: 5 * 60 * 1000,
};

// ── Pending /login verifications ─────────────────────────────
// Key: discordID → { newUID, oldUID, expiresAt }
const pendingLogin = new Map();

// ── HTTP helper ───────────────────────────────────────────────
function safeParseJSON(raw) {
  const safe = raw.replace(/:\s*(\d{16,})/g, ': "$1"');
  return JSON.parse(safe);
}

function fetchJSON(url, options = {}) {
  return new Promise((resolve, reject) => {
    const lib = url.startsWith("https") ? https : http;
    const method = options.method || "GET";
    const urlObj = new URL(url);
    const headers = {};
    if (method === "POST") headers["Content-Length"] = "0"; // Go server requires Content-Length on POSTs
    const reqOptions = {
      hostname: urlObj.hostname,
      port:     urlObj.port || (url.startsWith("https") ? 443 : 80),
      path:     urlObj.pathname + urlObj.search,
      method,
      headers,
    };
    const req = lib.request(reqOptions, (res) => {
      let raw = "";
      res.on("data", (c) => (raw += c));
      res.on("end", () => {
        try { resolve({ status: res.statusCode, body: safeParseJSON(raw) }); }
        catch (e) { reject(new Error(`Invalid JSON (HTTP ${res.statusCode})`)); }
      });
    });
    req.on("error", reject);
    req.setTimeout(8000, () => { req.destroy(); reject(new Error("Timed out")); });
    req.end();
  });
}

function apiURL(path) {
  return `${CONFIG.API_BASE}${CONFIG.API_PREFIX}/${CONFIG.BOT_USER_ID}/bot/${path}`;
}

async function fetchLeaderboard(lbID) {
  const { body } = await fetchJSON(apiURL(`query/leaderboard?leaderboard_id=${lbID}`));
  return body;
}

async function fetchServerStats() {
  const { body } = await fetchJSON(apiURL("server_stats"));
  return body;
}

async function fetchPlayerStats(userID) {
  try {
    const { body } = await fetchJSON(apiURL(`player_stats?user_id=${userID}`));
    if (body?.ok && body?.found) return body;
  } catch (_) {}
  return null;
}

async function fetchFTRankUpdates() {
  const tiersParam = CONFIG.FT_RANKS.map((t) => `${t.key}:${t.minScore}`).join(",");
  const { body } = await fetchJSON(apiURL(`ft_rank_updates?tiers=${encodeURIComponent(tiersParam)}&secret=${CONFIG.BOT_SECRET}`));
  return body;
}

async function banPlayer(userID) {
  const url = apiURL(`ban?user_id=${userID}&secret=${CONFIG.BOT_SECRET}`);
  const { body } = await fetchJSON(url, { method: "POST" });
  return body;
}

async function apiPost(path) {
  const url = apiURL(path);
  const { body } = await fetchJSON(url, { method: "POST" });
  return body;
}

async function apiGet(path) {
  const { body } = await fetchJSON(apiURL(path));
  return body;
}

// ── Helpers ───────────────────────────────────────────────────
function medal(rank) {
  if (rank === 1) return "🥇";
  if (rank === 2) return "🥈";
  if (rank === 3) return "🥉";
  return `**#${rank}**`;
}

function playerLabel(e, discordMap) {
  const uid = String(e.user_id ?? "");
  const name = e.name?.trim() || `User#${uid}`;
  const did = discordMap?.[uid];
  const dcTag = did ? ` <@${did}>` : "";
  return `**${name}**${dcTag} \`${uid}\``;
}

async function fetchDiscordMap(entries) {
  if (!entries?.length) return {};
  const ids = entries.map((e) => String(e.user_id)).join(",");
  try {
    const r = await apiGet(`discord_map?ids=${encodeURIComponent(ids)}&secret=${CONFIG.BOT_SECRET}`);
    return r?.ok ? (r.map ?? {}) : {};
  } catch {
    return {};
  }
}

function isAdmin(interaction) {
  const id = interaction.user.id;
  return id === CONFIG.OWNER_ID || CONFIG.ADMIN_IDS.includes(id);
}

async function resolveDiscordID(discordIDRaw) {
  const mentionMatch = String(discordIDRaw).match(/^<@!?(\d+)>$/);
  const discordID = mentionMatch ? mentionMatch[1] : String(discordIDRaw).trim();
  if (!/^\d+$/.test(discordID)) return { error: "❌ Invalid Discord ID — must be numbers only." };
  const linked = await apiGet(`is_registered?discord_id=${discordID}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
  if (!linked?.ok || !linked.registered)
    return { error: `❌ Discord user \`${discordID}\` has no linked game account.` };
  return { gameUID: String(linked.game_user_id) };
}

function fmt(n) {
  return Number(n ?? 0).toLocaleString();
}

function ftRankEmoji(key, guild) {
  const tier = CONFIG.FT_RANKS.find((t) => t.key === key);
  if (!tier) return key;
  const guildEmoji = guild?.emojis?.cache?.find((e) => e.name === tier.emoji);
  return guildEmoji ? `<:${guildEmoji.name}:${guildEmoji.id}>` : `[${tier.label}]`;
}

function ftTierForScore(score) {
  let best = null;
  for (const t of CONFIG.FT_RANKS) {
    if (score >= t.minScore) best = t;
  }
  return best;
}

// ── DM helper — silently tries to DM a user, returns true/false ──
async function tryDM(discordID, embed) {
  try {
    const user = await client.users.fetch(discordID);
    await user.send({ embeds: [embed] });
    return true;
  } catch {
    return false;
  }
}

// ── Ban duration helpers ──────────────────────────────────────
// Parses shorthand like "1s", "30m", "2h", "7d", or "permanent"
// Returns { seconds, label } or null if invalid.
function parseDuration(raw) {
  if (!raw) return null;
  const str = raw.trim().toLowerCase();
  if (str === "permanent" || str === "perm") return { seconds: 0, label: "**Permanent**" };

  const match = str.match(/^(\d+)\s*([smhd])$/);
  if (!match) return null;

  const amount = parseInt(match[1], 10);
  const unit   = match[2];
  if (amount <= 0) return null;

  const multipliers = { s: 1, m: 60, h: 3600, d: 86400 };
  const unitNames   = { s: amount === 1 ? "second" : "seconds",
                        m: amount === 1 ? "minute" : "minutes",
                        h: amount === 1 ? "hour"   : "hours",
                        d: amount === 1 ? "day"    : "days" };

  return {
    seconds: amount * multipliers[unit],
    label: `**${amount} ${unitNames[unit]}**`,
  };
}

function banUntilString(banUntilTs) {
  if (!banUntilTs) return "**Never (permanent ban)**";
  return `<t:${banUntilTs}:F> (<t:${banUntilTs}:R>)`;
}

// ── Resolve a target player (game ID, @mention, or discord_id) ──
// Returns { gameUID, discordID, username } or replies with an error.
async function resolveTarget(interaction, { userOption = "user", idOption = "userid", discordIDOption = "discord_id" } = {}) {
  let gameUID  = interaction.options.getString(idOption)?.trim() ?? null;
  let discordID = null;
  let username  = null;

  const mentionedUser = interaction.options.getUser(userOption);
  const rawDiscordID  = interaction.options.getString(discordIDOption)?.trim();

  if (mentionedUser) {
    const linked = await apiGet(`is_registered?discord_id=${mentionedUser.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
    if (!linked?.ok || !linked.registered) {
      await interaction.editReply(`❌ <@${mentionedUser.id}> has no linked game account.`);
      return null;
    }
    gameUID   = String(linked.game_user_id);
    discordID = mentionedUser.id;
    username  = linked.username;
  } else if (!gameUID && rawDiscordID) {
    const resolved = await resolveDiscordID(rawDiscordID);
    if (resolved.error) { await interaction.editReply(resolved.error); return null; }
    gameUID   = resolved.gameUID;
    discordID = rawDiscordID;
  }

  if (!gameUID || !/^\d+$/.test(gameUID)) {
    await interaction.editReply("❌ Provide a player game ID, @mention, or Discord ID.");
    return null;
  }

  // If we don't have discordID yet, try to look it up from game ID
  if (!discordID) {
    try {
      const linked = await apiGet(`linked_account_by_game_id?user_id=${gameUID}&secret=${CONFIG.BOT_SECRET}`);
      if (linked?.linked) {
        discordID = linked.discord_id ?? null;
        username  = username ?? linked.name;
      }
    } catch { /* no linked discord — that's fine */ }
  }

  return { gameUID, discordID, username };
}

// ── Player embed builder (shared by /player, /stats, etc.) ───
// Accepts optional playerStats if already fetched to avoid double-fetching.
async function fetchAndBuildPlayerEmbed(playerID, playerName, preloadedStats = null) {
  const [campaign, fastTrack, mpLb, playerStats] = await Promise.all([
    fetchLeaderboard("CAMPAIGN").catch(() => null),
    fetchLeaderboard("FAST_TRACK").catch(() => null),
    fetchLeaderboard("MP_GLOBAL").catch(() => null),
    preloadedStats !== undefined ? Promise.resolve(preloadedStats) : fetchPlayerStats(playerID).catch(() => null),
  ]);

  const isBanned   = !!playerStats?.banned;
  const banUntilTs = playerStats?.ban_until ?? 0; // 0 = permanent

  const findRank = (lb) => lb?.ok ? lb.ranking?.find((e) => String(e.user_id) === playerID) : null;

  const embed = new EmbedBuilder()
    .setColor(isBanned ? 0xe74c3c : 0x3498db)
    .setTitle(`🔍 ${playerName}`)
    .setDescription(`User ID: \`${playerID}\``);

  // Show ban status prominently at the top
  if (isBanned) {
    const until = banUntilTs ? `Expires: <t:${banUntilTs}:R>` : "Permanent";
    embed.addFields({ name: "🔨 Status", value: `**BANNED** — ${until}`, inline: false });
    // Hide rank info for banned players
    embed.addFields(
      { name: "🏆 Campaign",    value: "*Hidden while banned*" },
      { name: "🚀 Fast Track",  value: "*Hidden while banned*" },
      { name: "⚔️ Multiplayer", value: "*Hidden while banned*" },
    );
  } else {
    embed.addFields({ name: "✅ Status", value: "Active", inline: false });
    const cp = findRank(campaign);
    const ft = findRank(fastTrack);
    const mp = findRank(mpLb);
    embed.addFields(
      cp ? { name: "🏆 Campaign",    value: `Rank **#${cp.rank}** — **${fmt(cp.score)} pts**`   }
         : { name: "🏆 Campaign",    value: "Not ranked yet" },
      ft ? { name: "🚀 Fast Track",  value: `Rank **#${ft.rank}** — **${fmt(ft.score)} coins**` }
         : { name: "🚀 Fast Track",  value: "Not ranked yet" },
      mp ? { name: "⚔️ Multiplayer", value: `Rank **#${mp.rank}** — **${fmt(mp.score)} pts**`   }
         : { name: "⚔️ Multiplayer", value: "Not ranked yet" },
    );
  }

  if (playerStats && !isBanned) {
    embed.addFields(
      { name: "💰 Coins",        value: fmt(playerStats.coins),                     inline: true },
      { name: "💎 Gems",         value: fmt(playerStats.gems),                      inline: true },
      { name: "🎮 Score MP",     value: fmt(playerStats.score_mp) + " pts",         inline: true },
      { name: "🚀 FT Max Coins", value: fmt(playerStats.fast_track_coins) + " 🪙",  inline: true },
      { name: "📈 Max Level",    value: String(playerStats.max_level_reached ?? 0), inline: true },
    );
  }

  return embed.setTimestamp();
}

// ── Commands ──────────────────────────────────────────────────
const commands = [
  new SlashCommandBuilder()
    .setName("leaderboard")
    .setDescription("🏆 Top players leaderboard")
    .addStringOption((o) =>
      o.setName("type").setDescription("Leaderboard type (default: Campaign)").setRequired(false)
        .addChoices(
          { name: "Campaign (Score)",       value: "CAMPAIGN"   },
          { name: "Fast Track (Max Coins)", value: "FAST_TRACK" },
          { name: "Multiplayer (MP Score)", value: "MP_GLOBAL"  },
        )
    )
    .addIntegerOption((o) =>
      o.setName("top").setDescription("Number of players to show (1–25, default 10)").setRequired(false).setMinValue(1).setMaxValue(25)
    ),

  new SlashCommandBuilder()
    .setName("top3")
    .setDescription("🎖️ Top 3 Campaign players podium"),

  new SlashCommandBuilder()
    .setName("player")
    .setDescription("🔍 Look up a player — or use with no arguments to see your own private profile")
    .addStringOption((o) =>
      o.setName("search").setDescription("Username or game user ID (auto-detected)").setRequired(false)
    )
    .addUserOption((o) => o.setName("user").setDescription("@mention a Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID (if not mentioning)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("compare")
    .setDescription("⚔️ Head-to-head comparison between two players")
    .addStringOption((o) => o.setName("player1").setDescription("First player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user1").setDescription("First player @mention").setRequired(false))
    .addStringOption((o) => o.setName("discord_id1").setDescription("First player Discord user ID").setRequired(false))
    .addStringOption((o) => o.setName("player2").setDescription("Second player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user2").setDescription("Second player @mention").setRequired(false))
    .addStringOption((o) => o.setName("discord_id2").setDescription("Second player Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("stats")
    .setDescription("📊 Server stats: players, scores and activity"),

  new SlashCommandBuilder()
    .setName("online")
    .setDescription("🟢 Players online right now"),

  new SlashCommandBuilder()
    .setName("users")
    .setDescription("👥 Total registered players and new signups today"),

  new SlashCommandBuilder()
    .setName("ping")
    .setDescription("🏓 Check if the game server is online"),

  // ── Moderation ────────────────────────────────────────────────
  new SlashCommandBuilder()
    .setName("ban")
    .setDescription("🔨 [Admin] Ban a player — account stays, hidden from leaderboards")
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false))
    .addStringOption((o) =>
      o.setName("duration")
        .setDescription("Duration: 30s · 10m · 2h · 7d · permanent — omit for permanent ban")
        .setRequired(false)
    )
    .addStringOption((o) => o.setName("reason").setDescription("Reason (shown to the player in their DM)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("unban")
    .setDescription("✅ [Admin] Remove a ban from a player")
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("wipe")
    .setDescription("💥 [Admin] PERMANENTLY delete all of a player's game data — cannot be undone")
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false))
    .addStringOption((o) => o.setName("reason").setDescription("Reason (shown to the player in their DM)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("unlink")
    .setDescription("🔗 [Admin] Unlink a Discord account from its game ID")
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("whitelist")
    .setDescription("✅ [Admin] Whitelist a player — bypasses cheat detection")
    .addStringOption((o) => o.setName("action").setDescription("Add or remove").setRequired(true)
      .addChoices({ name: "Add", value: "add" }, { name: "Remove", value: "remove" }))
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false))
    .addStringOption((o) => o.setName("note").setDescription("Reason (optional)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("blacklist")
    .setDescription("🚫 [Admin] Ban or unban a device UID")
    .addStringOption((o) => o.setName("device_uid").setDescription("Device UID — use /devices to find it").setRequired(true))
    .addStringOption((o) => o.setName("action").setDescription("Ban or unban").setRequired(true)
      .addChoices({ name: "Ban device", value: "add" }, { name: "Unban device", value: "remove" }))
    .addStringOption((o) => o.setName("player_id").setDescription("Player game ID to notify via DM (optional)").setRequired(false))
    .addStringOption((o) => o.setName("reason").setDescription("Reason (optional)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("devices")
    .setDescription("📱 [Admin] List all devices a player has used")
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(false))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("registered")
    .setDescription("🔗 [Admin] List all Discord-linked accounts")
    .addIntegerOption((o) => o.setName("page").setDescription("Page number (default 1)").setRequired(false).setMinValue(1)),

  new SlashCommandBuilder()
    .setName("isregistered")
    .setDescription("🔍 [Admin] Check which game account a Discord user has linked")
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("gift")
    .setDescription("🎁 [Admin] Gift coins, gems, or lives to a player")
    .addStringOption((o) =>
      o.setName("type").setDescription("What to gift").setRequired(true)
        .addChoices(
          { name: "Coins 💰", value: "coins" },
          { name: "Gems 💎",  value: "gems"  },
          { name: "Lives ❤️", value: "lives" },
        )
    )
    .addIntegerOption((o) =>
      o.setName("amount").setDescription("How many to give (must be positive)").setRequired(true).setMinValue(1).setMaxValue(9999999)
    )
    .addStringOption((o) => o.setName("userid").setDescription("Player game ID").setRequired(true))
    .addUserOption((o) => o.setName("user").setDescription("@mention Discord user (overrides userid)").setRequired(false))
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID (overrides userid)").setRequired(false)),

  new SlashCommandBuilder()
    .setName("register")
    .setDescription("🔗 Link your Discord to your Dragon Land account")
    .addStringOption((o) =>
      o.setName("userid").setDescription("Your in-game user ID").setRequired(true)
    ),

  new SlashCommandBuilder()
    .setName("login")
    .setDescription("🔄 Recover your account to a new user ID after reinstalling")
    .addStringOption((o) =>
      o.setName("newuserid").setDescription("Your new in-game user ID").setRequired(true)
    ),

  new SlashCommandBuilder()
    .setName("ftrank")
    .setDescription("🏅 Check your Fast Track rank or look up another player's rank")
    .addUserOption((o) => o.setName("user").setDescription("@mention a Discord user").setRequired(false))
    .addStringOption((o) =>
      o.setName("userid").setDescription("Game user ID to look up (leave blank for your own rank)").setRequired(false)
    )
    .addStringOption((o) => o.setName("discord_id").setDescription("Discord user ID").setRequired(false)),

  new SlashCommandBuilder()
    .setName("help")
    .setDescription("📖 List all commands"),

].map((c) => c.toJSON());

// ── Register commands ─────────────────────────────────────────
async function registerCommands() {
  const rest = new REST({ version: "10" }).setToken(CONFIG.TOKEN);
  try {
    console.log("Registering slash commands...");
    await rest.put(Routes.applicationGuildCommands(CONFIG.CLIENT_ID, CONFIG.GUILD_ID), { body: commands });
    console.log("✅ Commands registered to guild.");
  } catch (err) {
    console.error("Failed to register commands:", err);
  }
}

// ── Client ────────────────────────────────────────────────────
const client = new Client({
  intents: [
    GatewayIntentBits.Guilds,
    GatewayIntentBits.GuildMessages,
    GatewayIntentBits.MessageContent,
    GatewayIntentBits.DirectMessages,
  ],
  partials: [Partials.Channel],
});

client.once("ready", () => {
  console.log(`✅ Logged in as ${client.user.tag}`);
  client.user.setPresence({
    activities: [{ name: "Dragon Land", type: ActivityType.Playing }],
    status: "online",
  });

  if (CONFIG.FT_RANK_ANNOUNCE_CHANNEL && CONFIG.FT_RANK_ANNOUNCE_CHANNEL !== "YOUR_RANK_UPS_CHANNEL_ID_HERE") {
    client.channels.fetch(CONFIG.FT_RANK_ANNOUNCE_CHANNEL)
      .then((ch) => console.log(`✅ FT rank-up channel confirmed: #${ch.name}`))
      .catch(() => console.error(`❌ FT_RANK_ANNOUNCE_CHANNEL "${CONFIG.FT_RANK_ANNOUNCE_CHANNEL}" not found.`));
  } else {
    console.warn("⚠️  FT_RANK_ANNOUNCE_CHANNEL not set — rank-up announcements will be DM-only.");
  }

  async function pollFTRankUps() {
    try {
      const data = await fetchFTRankUpdates();
      if (!data?.ok || !data.upgrades?.length) return;

      const guild = client.guilds.cache.get(CONFIG.GUILD_ID);
      let announceChannel = null;
      if (CONFIG.FT_RANK_ANNOUNCE_CHANNEL && CONFIG.FT_RANK_ANNOUNCE_CHANNEL !== "YOUR_RANK_UPS_CHANNEL_ID_HERE") {
        try { announceChannel = await client.channels.fetch(CONFIG.FT_RANK_ANNOUNCE_CHANNEL); }
        catch (err) { console.error(`FT poll: could not fetch announce channel: ${err.message}`); }
      }

      for (const u of data.upgrades) {
        const tier = CONFIG.FT_RANKS.find((t) => t.key === u.new_rank_key);
        if (!tier) continue;

        const emojiStr = ftRankEmoji(u.new_rank_key, guild);
        const username = u.username || `User#${u.game_user_id}`;
        const scoreStr = Number(u.score).toLocaleString();

        if (announceChannel) {
          try {
            const embed = new EmbedBuilder()
              .setColor(tier.color)
              .setTitle(`${emojiStr} New Fast Track ${tier.label}!`)
              .setDescription(
                `<@${u.discord_id}> just reached **${tier.label}** rank in Fast Track!\n\n` +
                `🪙 Score: **${scoreStr} coins**`
              )
              .addFields({ name: "Player", value: `**${username}** \`${u.game_user_id}\``, inline: true })
              .setFooter({ text: "Fast Track Ranked" })
              .setTimestamp();
            await announceChannel.send({ content: `<@${u.discord_id}>`, embeds: [embed] });
          } catch (err) {
            console.error("FT rank announce error:", err.message);
          }
        }

        // DM the player about their rank-up
        if (u.discord_id) {
          await tryDM(u.discord_id, new EmbedBuilder()
            .setColor(tier.color)
            .setTitle(`${emojiStr} Fast Track Rank Up!`)
            .setDescription(
              `Congrats **${username}**! You've reached **${tier.label}** rank in Fast Track!\n\n` +
              `🪙 Your score: **${scoreStr} coins**\n\n` +
              `Keep going — the next rank awaits! 🐉`
            )
            .setTimestamp()
          );
        }

        await new Promise((r) => setTimeout(r, 500));
      }
    } catch (err) {
      console.error("FT rank poll error:", err.message);
    }
  }

  pollFTRankUps();
  setInterval(pollFTRankUps, CONFIG.FT_RANK_POLL_INTERVAL_MS);
});

// ── DM listener — handles verification code replies ───────────
client.on("messageCreate", async (message) => {
  if (message.author.bot) return;

  if (message.guild) {
    const emojiName = CONFIG.MEME_EMOJI_NAME;
    if (emojiName && emojiName !== "roggy") {
      const regex = new RegExp(`<a?:${emojiName}:\\d+>`, "g");
      const matches = message.content.match(regex) || [];
      if (matches.length >= CONFIG.MEME_EMOJI_THRESHOLD) {
        try { await message.react("💀"); }
        catch (err) { console.error("Failed to react:", err.message); }
      }
    }
    return;
  }

  const code = message.content.trim();
  if (!/^\d{6}$/.test(code)) return;

  const discordID = message.author.id;

  // ── Check if this code is for a pending /login migration ──
  const pendingMigration = pendingLogin.get(discordID);
  if (pendingMigration) {
    // Expired? Clean up and bail.
    if (Date.now() > pendingMigration.expiresAt) {
      pendingLogin.delete(discordID);
      return message.reply(
        `⏰ **Migration code expired.** Your accounts are unchanged.\n\n` +
        `Run \`/login ${pendingMigration.newUID}\` again to start a fresh verification.`
      );
    }

    try {
      // verify_code confirms ownership of newUID and links it to this Discord.
      // The server will unlink it from newUID right after so the migrate call
      // can re-link it properly — or migrate can use the verified session directly.
      const verifyResult = await apiPost(
        `verify_code?discord_id=${discordID}&code=${code}&secret=${CONFIG.BOT_SECRET}&migration=true`
      );

      if (!verifyResult?.ok) {
        const error = verifyResult?.error;
        if (error === "wrong_code") {
          const rem = verifyResult?.attempts_remaining ?? "?";
          return message.reply(
            `❌ **Wrong code.** ${rem} attempt(s) remaining.\n` +
            `Check the number shown in your **lives counter** in-game and try again.`
          );
        }
        if (error === "max_attempts") {
          pendingLogin.delete(discordID);
          return message.reply(
            `🔒 **Too many wrong attempts.** The migration has been cancelled and your lives have been restored.\n\n` +
            `Run \`/login ${pendingMigration.newUID}\` again to start over.`
          );
        }
        if (error === "code_expired") {
          pendingLogin.delete(discordID);
          return message.reply(
            `⏰ **Code expired.** The migration has been cancelled and your lives have been restored.\n\n` +
            `Run \`/login ${pendingMigration.newUID}\` again to get a fresh code.`
          );
        }
        pendingLogin.delete(discordID);
        return message.reply(`❌ Verification failed: ${verifyResult?.msg ?? verifyResult?.error ?? "Unknown error."}\n\nRun \`/login\` again to retry.`);
      }

      // Ownership confirmed — now execute the actual migration
      pendingLogin.delete(discordID);
      const migrateResult = await apiPost(
        `migrate?discord_id=${discordID}&new_user_id=${pendingMigration.newUID}&secret=${CONFIG.BOT_SECRET}`
      );

      if (!migrateResult?.ok) {
        return message.reply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("❌ Migration Failed")
            .setDescription(
              `Your code was verified, but the migration could not complete: **${migrateResult?.msg ?? migrateResult?.error ?? "unknown error"}**\n\n` +
              `✅ **Your Discord account is still linked to your original account \`${pendingMigration.oldUID}\`** — nothing was lost.\n\n` +
              `You can run \`/login ${pendingMigration.newUID}\` again to retry, or contact a server admin if the problem persists.`
            ).setTimestamp()],
        });
      }

      return message.reply({
        embeds: [new EmbedBuilder()
          .setColor(0x2ecc71)
          .setTitle("✅ Account Migrated Successfully!")
          .setDescription(
            `All data from \`${migrateResult.old_user ?? pendingMigration.oldUID}\` has been moved to \`${migrateResult.new_user ?? pendingMigration.newUID}\`.\n\n` +
            `Account \`${migrateResult.old_user ?? pendingMigration.oldUID}\` has been deleted.\n\n` +
            `Your Discord is now linked to \`${migrateResult.new_user ?? pendingMigration.newUID}\`. Welcome back! 🐉\n\n` +
            `Dragon Land will reconnect automatically — your restored progress will be there when it does.`
          )
          .setTimestamp()],
      });

    } catch (err) {
      console.error("login verify_code / migrate error:", err);
      pendingLogin.delete(discordID);
      return message.reply("❌ Server error during migration. Please try again or contact a server admin.");
    }
  }

  // ── Regular /register code verification ───────────────────
  try {
    const result = await apiPost(
      `verify_code?discord_id=${discordID}&code=${code}&secret=${CONFIG.BOT_SECRET}`
    );

    if (result?.ok) {
      await message.reply(
        `✅ **Account linked!** Your Discord is now linked to game account **${result.username}** (\`${result.linked_user}\`).\n\n` +
        `🔄 If you ever reinstall the game, use \`/login <new_user_id>\` in the server to recover all your progress.`
      );
    } else {
      const error = result?.error;
      const msg   = result?.msg || result?.error || "Something went wrong.";

      if (error === "wrong_code") {
        const rem = result?.attempts_remaining ?? "?";
        await message.reply(
          `❌ **Wrong code.** ${rem} attempt(s) remaining.\n` +
          `Check the number shown in your **lives counter** in-game and try again.`
        );
      } else if (error === "max_attempts") {
        await message.reply(
          `🔒 **Too many wrong attempts.** Your lives have been restored and the code has been cancelled.\n\n` +
          `Use \`/register <user_id>\` again to start over and get a new code.`
        );
      } else if (error === "code_expired") {
        await message.reply(
          `⏰ **Code expired.** Codes are only valid for 5 minutes and your lives have been restored.\n\n` +
          `Use \`/register <user_id>\` again to get a fresh code.`
        );
      } else if (error === "no_pending_code") {
        await message.reply(
          `❓ **No active code found.** Use \`/register <user_id>\` in the server first, then reply here with the 6-digit code shown in your lives counter.`
        );
      } else {
        await message.reply(`❌ ${msg}`);
      }
    }
  } catch (err) {
    console.error("verify_code DM error:", err);
    await message.reply("❌ Server error. Try again.");
  }
});

// ── resolveDiscordTarget ──────────────────────────────────────
async function resolveDiscordTarget(interaction, optionName) {
  let discordID = null;

  const mentionResolved = interaction.options.resolved?.users?.get(
    interaction.options.data.find(o => o.name === optionName)?.value
  );
  if (mentionResolved) {
    discordID = mentionResolved.id;
  } else {
    const raw = interaction.options.getString(optionName)?.trim() ?? "";
    const mentionMatch = raw.match(/^<@!?(\d+)>$/);
    if (mentionMatch) {
      discordID = mentionMatch[1];
    } else if (/^\d+$/.test(raw)) {
      discordID = raw;
    }
  }

  if (discordID) {
    const r = await apiGet(`is_registered?discord_id=${discordID}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
    if (r?.ok && r.registered) {
      return { discordID, gameUID: String(r.game_user_id), username: r.username };
    }
    const raw = interaction.options.getString(optionName)?.trim() ?? "";
    if (/^\d+$/.test(raw) && raw.length <= 12) {
      return { discordID: null, gameUID: raw, username: null };
    }
    return { discordID, gameUID: null, username: null };
  }
  return null;
}

// ── Slash command handler ─────────────────────────────────────
client.on("interactionCreate", async (interaction) => {
  if (!interaction.isChatInputCommand()) return;
  const { commandName } = interaction;

  // ── /help ─────────────────────────────────────────────────
  if (commandName === "help") {
    const embed = new EmbedBuilder()
      .setColor(0xff6600)
      .setTitle("Dragon Land Bot — All Commands")
      .addFields(
        { name: "🏆 `/leaderboard [type] [top]`",         value: "Top players by Campaign, Fast Track, or MP score." },
        { name: "🎖️ `/top3`",                             value: "Campaign podium — top 3 players." },
        { name: "🔍 `/player [search]`",                   value: "Look up any player. No arguments = your own private stats." },
        { name: "⚔️ `/compare`",                           value: "Head-to-head score comparison." },
        { name: "📊 `/stats`",                             value: "Full server stats." },
        { name: "🟢 `/online`",                            value: "Players active right now." },
        { name: "👥 `/users`",                             value: "Total players and new signups today." },
        { name: "🏓 `/ping`",                              value: "Check if the game server is online." },
        { name: "🔗 `/register <userid>`",                 value: "Link your Discord to your Dragon Land account." },
        { name: "🔄 `/login <new_user_id>`",               value: "Recover your account after reinstalling." },
        { name: "🏅 `/ftrank [userid]`",                   value: "Your Fast Track rank and progress." },
        { name: "🔨 `/ban <userid> <duration>`",           value: "**[Admin]** Soft-ban a player. Account stays, hidden from leaderboards." },
        { name: "✅ `/unban <userid>`",                    value: "**[Admin]** Remove a ban from a player." },
        { name: "💥 `/wipe <userid>`",                     value: "**[Admin]** Permanently erase all of a player's game data." },
        { name: "🔗 `/unlink <userid>`",                   value: "**[Admin]** Unlink a Discord account from its game ID." },
        { name: "🎁 `/gift <type> <amount> <userid>`",     value: "**[Admin]** Gift coins, gems, or lives to a player." },
        { name: "✅ `/whitelist <userid> <add|remove>`",   value: "**[Admin]** Exempt a player from cheat detection." },
        { name: "🚫 `/blacklist <device_uid> <add|remove>`",value: "**[Admin]** Block a device UID." },
        { name: "📱 `/devices <userid>`",                  value: "**[Admin]** List all devices a player has used." },
        { name: "🔗 `/registered [page]`",                 value: "**[Admin]** List all Discord-linked accounts." },
        { name: "🔍 `/isregistered <@user>`",              value: "**[Admin]** Check which game account a Discord user has linked." },
        { name: "❓ `/help`",                              value: "Show this message." },
      )
      .setFooter({ text: "Dragon Land Bot" })
      .setTimestamp();
    return interaction.reply({ embeds: [embed] });
  }

  // ── /ping ─────────────────────────────────────────────────
  if (commandName === "ping") {
    await interaction.deferReply();
    const start = Date.now();
    try {
      const { status, body } = await fetchJSON(`${CONFIG.API_BASE}/health`);
      const ms = Date.now() - start;
      const ok = status === 200 && body?.status === "ok";
      const embed = new EmbedBuilder()
        .setColor(ok ? 0x00cc66 : 0xff9900)
        .setTitle("🏓 Pong!")
        .addFields(
          { name: "Game Server", value: ok ? "✅ Online" : "⚠️ Degraded", inline: true },
          { name: "API Latency", value: `${ms}ms`,                         inline: true },
          { name: "Bot Latency", value: `${Math.round(client.ws.ping)}ms`, inline: true },
        )
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply({
        embeds: [new EmbedBuilder().setColor(0xff0000).setTitle("🏓 Pong!")
          .setDescription("❌ Game server **unreachable**.").setTimestamp()],
      });
    }
  }

  // ── /leaderboard ──────────────────────────────────────────
  if (commandName === "leaderboard") {
    await interaction.deferReply();
    const lbType = interaction.options.getString("type") ?? "CAMPAIGN";
    const topN   = interaction.options.getInteger("top")  ?? 10;

    const labelMap = { CAMPAIGN: "Campaign 🏆", FAST_TRACK: "Fast Track 🚀", MP_GLOBAL: "Multiplayer ⚔️" };
    const unitMap  = { CAMPAIGN: "pts",         FAST_TRACK: "coins",          MP_GLOBAL: "pts"            };
    const colorMap = { CAMPAIGN: 0xf1c40f,      FAST_TRACK: 0x9b59b6,        MP_GLOBAL: 0xe74c3c         };

    const label = labelMap[lbType] ?? lbType;
    const unit  = unitMap[lbType]  ?? "pts";
    const color = colorMap[lbType] ?? 0x1abc9c;

    try {
      const data = await fetchLeaderboard(lbType);
      if (!data?.ok || !data.ranking?.length)
        return interaction.editReply("📭 No leaderboard data yet!");
      const slice = data.ranking.slice(0, topN);
      const discordMap = await fetchDiscordMap(slice);
      const rows = slice.map((e) =>
        `${medal(e.rank)} ${playerLabel(e, discordMap)} — ${fmt(e.score)} ${unit}`
      ).join("\n");
      const embed = new EmbedBuilder()
        .setColor(color)
        .setTitle(`${label} Leaderboard — Top ${Math.min(topN, data.ranking.length)}`)
        .setDescription(rows)
        .setFooter({ text: `${data.total ?? data.ranking.length} players ranked total · Banned players are hidden` })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Failed to fetch leaderboard.");
    }
  }

  // ── /top3 ─────────────────────────────────────────────────
  if (commandName === "top3") {
    await interaction.deferReply();
    try {
      const data = await fetchLeaderboard("CAMPAIGN");
      if (!data?.ok || !data.ranking?.length)
        return interaction.editReply("📭 No players on the leaderboard yet!");
      const podiumSlice = data.ranking.slice(0, 3);
      const discordMap  = await fetchDiscordMap(podiumSlice);
      const podium = podiumSlice.map((e) =>
        `${medal(e.rank)} ${playerLabel(e, discordMap)}\n> Score: **${fmt(e.score)}** pts`
      ).join("\n\n");
      const embed = new EmbedBuilder()
        .setColor(0xf1c40f).setTitle("🎖️ Campaign Podium")
        .setDescription(podium).setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /player ───────────────────────────────────────────────
  if (commandName === "player") {
    const playerMention   = interaction.options.getUser("user");
    const playerDiscordID = interaction.options.getString("discord_id")?.trim();
    const search          = interaction.options.getString("search")?.trim();

    const isSelfProfile = !playerMention && !playerDiscordID && !search;
    await interaction.deferReply({ ephemeral: isSelfProfile });

    if (isSelfProfile) {
      try {
        const linked = await apiGet(`linked_account?discord_id=${interaction.user.id}&secret=${CONFIG.BOT_SECRET}`);
        if (!linked?.linked) {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0x95a5a6)
              .setTitle("🔗 No Linked Account")
              .setDescription(
                `You haven't linked your Discord to a Dragon Land account yet.\n\n` +
                `Complete your first level, then use \`/register <your_user_id>\`.`
              ).setTimestamp()],
          });
        }
        const uid = String(linked.user_id);
        const embed = (await fetchAndBuildPlayerEmbed(uid, linked.name ?? `User#${uid}`))
          .setColor(0x9b59b6)
          .setDescription(`Game ID: \`${uid}\`  ·  Only you can see this`)
          .setFooter({ text: "Only visible to you · Use /player <name or ID> to look up others" });
        return interaction.editReply({ embeds: [embed] });
      } catch (err) {
        console.error("/player (self) error:", err);
        return interaction.editReply("❌ Server error.");
      }
    }

    try {
      let gameUID = null;

      if (playerMention || playerDiscordID) {
        const resolved = await resolveDiscordID(playerMention ? playerMention.id : playerDiscordID);
        if (resolved.error) return interaction.editReply(resolved.error);
        gameUID = resolved.gameUID;
        const searchResult = await apiGet(`search_player?user_id=${gameUID}`);
        if (!searchResult?.ok || !searchResult.results?.length)
          return interaction.editReply(`❌ No player found with game ID \`${gameUID}\`.`);
        const p = searchResult.results[0];
        const embed = await fetchAndBuildPlayerEmbed(String(p.user_id), p.username?.trim() || `User#${p.user_id}`);
        return interaction.editReply({ embeds: [embed] });
      }

      const isIDSearch  = /^\d+$/.test(search);
      const searchResult = await apiGet(
        isIDSearch ? `search_player?user_id=${search}` : `search_player?q=${encodeURIComponent(search)}`
      );

      if (!searchResult?.ok || !searchResult.results?.length) {
        return interaction.editReply(
          isIDSearch
            ? `❌ No player found with ID \`${search}\`.`
            : `❌ No player found matching **"${search}"**.`
        );
      }

      if (searchResult.results.length > 1) {
        const list = searchResult.results.map((r) => {
          const name = r.username?.trim() || `User#${r.user_id}`;
          return `• **${name}** \`${r.user_id}\``;
        }).join("\n");
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0x3498db)
            .setTitle(`🔍 ${searchResult.results.length} players found for "${search}"`)
            .setDescription(list + "\n\nPaste one of the IDs above into `/player search` to see their full stats.")
            .setTimestamp()],
        });
      }

      const p = searchResult.results[0];
      const embed = await fetchAndBuildPlayerEmbed(String(p.user_id), p.username?.trim() || `User#${p.user_id}`);
      return interaction.editReply({ embeds: [embed] });

    } catch (err) {
      console.error("/player error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /compare ──────────────────────────────────────────────
  if (commandName === "compare") {
    await interaction.deferReply();
    let id1 = interaction.options.getString("player1")?.trim();
    let id2 = interaction.options.getString("player2")?.trim();
    const cmpMention1   = interaction.options.getUser("user1");
    const cmpMention2   = interaction.options.getUser("user2");
    const cmpDiscordID1 = interaction.options.getString("discord_id1")?.trim();
    const cmpDiscordID2 = interaction.options.getString("discord_id2")?.trim();
    try {
      if (cmpMention1) {
        const l = await apiGet(`is_registered?discord_id=${cmpMention1.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
        if (!l?.ok || !l.registered) return interaction.editReply(`❌ <@${cmpMention1.id}> has no linked game account.`);
        id1 = String(l.game_user_id);
      } else if (!id1 && cmpDiscordID1) {
        const resolved = await resolveDiscordID(cmpDiscordID1);
        if (resolved.error) return interaction.editReply(resolved.error);
        id1 = resolved.gameUID;
      }
      if (cmpMention2) {
        const l = await apiGet(`is_registered?discord_id=${cmpMention2.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
        if (!l?.ok || !l.registered) return interaction.editReply(`❌ <@${cmpMention2.id}> has no linked game account.`);
        id2 = String(l.game_user_id);
      } else if (!id2 && cmpDiscordID2) {
        const resolved = await resolveDiscordID(cmpDiscordID2);
        if (resolved.error) return interaction.editReply(resolved.error);
        id2 = resolved.gameUID;
      }
      if (!id1 || !/^\d+$/.test(id1)) return interaction.editReply("❌ Provide player 1 as a game ID or @mention.");
      if (!id2 || !/^\d+$/.test(id2)) return interaction.editReply("❌ Provide player 2 as a game ID or @mention.");
      const data = await fetchLeaderboard("CAMPAIGN");
      if (!data?.ok || !data.ranking) return interaction.editReply("❌ Could not fetch leaderboard.");
      const find   = (id) => data.ranking.find((e) => String(e.user_id) === id);
      const p1     = find(id1);
      const p2     = find(id2);
      if (!p1 && !p2) return interaction.editReply("❌ Neither player found on the leaderboard.");
      const name1  = (p1?.name || "").trim() || `Player ${id1}`;
      const name2  = (p2?.name || "").trim() || `Player ${id2}`;
      const score1 = p1 ? Number(p1.score) : 0;
      const score2 = p2 ? Number(p2.score) : 0;
      const maxS   = Math.max(score1, score2, 1);
      const bar    = (s) => { const f = Math.round((s/maxS)*10); return "█".repeat(f)+"░".repeat(10-f); };
      const winner = score1 > score2 ? name1 : score2 > score1 ? name2 : null;
      const embed  = new EmbedBuilder()
        .setColor(0xe74c3c)
        .setTitle(`⚔️ ${name1} vs ${name2}`)
        .addFields(
          { name: "🏆 Campaign Score",
            value: `**${name1}** (Rank #${p1?.rank ?? "N/A"})\n\`${bar(score1)}\` ${score1.toLocaleString()} pts\n\n`
                 + `**${name2}** (Rank #${p2?.rank ?? "N/A"})\n\`${bar(score2)}\` ${score2.toLocaleString()} pts` },
          { name: "🎉 Result",
            value: winner ? `**${winner}** wins by **${Math.abs(score1-score2).toLocaleString()}** points!` : "It's a **tie**!" },
        )
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /online ───────────────────────────────────────────────
  if (commandName === "online") {
    await interaction.deferReply();
    try {
      const s = await fetchServerStats();
      if (!s?.ok) return interaction.editReply("❌ Could not fetch stats.");
      const count = Number(s.online);
      const embed = new EmbedBuilder()
        .setColor(count > 0 ? 0x00cc66 : 0x99aab5)
        .setTitle("🟢 Players Online")
        .setDescription(count > 0 ? `**${count}** player${count === 1 ? "" : "s"} active in the last 2 minutes` : "No players active right now")
        .setFooter({ text: "Based on last game packet received" })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Could not reach the server.");
    }
  }

  // ── /users ────────────────────────────────────────────────
  if (commandName === "users") {
    await interaction.deferReply();
    try {
      const s = await fetchServerStats();
      if (!s?.ok) return interaction.editReply("❌ Could not fetch stats.");
      const embed = new EmbedBuilder()
        .setColor(0x3498db)
        .setTitle("👥 Registered Players")
        .addFields(
          { name: "Total Players",      value: `**${fmt(s.total)}**`,        inline: true },
          { name: "New Today",          value: `**${fmt(s.new_today)}**`,    inline: true },
          { name: "Online Now",         value: `**${fmt(s.online)}**`,       inline: true },
          { name: "Peak CCU (24h)",     value: `**${fmt(s.peak_ccu_24h)}**`, inline: true },
          { name: "Peak CCU (All-time)",value: `**${fmt(s.peak_ccu)}**`,     inline: true },
        )
        .setFooter({ text: "Live from game server" })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /stats ────────────────────────────────────────────────
  if (commandName === "stats") {
    await interaction.deferReply();
    try {
      const [campaign, fastTrack, mpLb, s] = await Promise.all([
        fetchLeaderboard("CAMPAIGN"),
        fetchLeaderboard("FAST_TRACK"),
        fetchLeaderboard("MP_GLOBAL"),
        fetchServerStats(),
      ]);
      const tc = campaign?.ranking?.[0];
      const tf = fastTrack?.ranking?.[0];
      const tm = mpLb?.ranking?.[0];
      const topStats = tc ? await fetchPlayerStats(String(tc.user_id)) : null;
      const topEntries = [tc, tf, tm].filter(Boolean);
      const statsDiscordMap = await fetchDiscordMap(topEntries);

      const embed = new EmbedBuilder()
        .setColor(0x1abc9c)
        .setTitle("📊 Dragon Land Server Stats")
        .addFields(
          { name: "👥 Players", value: s?.ok ? `**${fmt(s.total)}** registered · **${s.online}** online now · **${s.new_today}** new today` : "Unavailable" },
          { name: "📈 Peak CCU", value: s?.ok ? `**${fmt(s.peak_ccu)}** all-time · **${fmt(s.peak_ccu_24h)}** past 24h` : "Unavailable" },
          { name: "🏆 Campaign", value: tc ? `**${campaign.total ?? campaign.ranking.length}** ranked · Top: ${playerLabel(tc, statsDiscordMap)} — **${fmt(tc.score)} pts**` : "No data yet" },
          { name: "🚀 Fast Track (Max Coins)", value: tf ? `**${fastTrack.total ?? fastTrack.ranking.length}** ranked · Most coins: **${fmt(tf.score)} 🪙** by ${playerLabel(tf, statsDiscordMap)}` : "No data yet" },
          { name: "⚔️ Multiplayer", value: tm ? `**${mpLb.total ?? mpLb.ranking.length}** ranked · Top: ${playerLabel(tm, statsDiscordMap)} — **${fmt(tm.score)} pts**` : "No data yet" },
        );

      if (topStats) {
        embed.addFields({
          name: `🏅 Top Player Snapshot (${tc?.name?.trim() || ("User#" + tc?.user_id)})`,
          value: [
            `💰 Total Coins: **${fmt(topStats.coins)}**`,
            `💎 Gems: **${fmt(topStats.gems)}**`,
            `🎮 Score MP: **${fmt(topStats.score_mp)} pts**`,
            `🚀 FT Max Coins: **${fmt(topStats.fast_track_coins)}**`,
            `📈 Max Level: **${topStats.max_level_reached ?? 0}**`,
          ].join("  ·  "),
        });
      }

      embed.setFooter({ text: "Live from game server" }).setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch {
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /ban (soft ban — account preserved) ───────────────────
  if (commandName === "ban") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Only the bot owner or admins can use this command.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    const target = await resolveTarget(interaction);
    if (!target) return;
    const { gameUID, discordID } = target;

    const durationRaw = (interaction.options.getString("duration") ?? "permanent").trim();
    const reason      = interaction.options.getString("reason") ?? "No reason provided.";

    // Parse the shorthand duration (e.g. "7d", "2h", "30m", "permanent")
    const parsed = parseDuration(durationRaw);
    if (!parsed) {
      return interaction.editReply(
        `❌ Invalid duration **"${durationRaw}"**. Use formats like \`30s\`, \`10m\`, \`2h\`, \`7d\`, or \`permanent\`.`
      );
    }

    const durationSeconds = parsed.seconds;
    const durationLabel   = parsed.label;

    try {
      const result = await apiPost(
        `ban_session?user_id=${gameUID}&duration_seconds=${durationSeconds}&reason=${encodeURIComponent(reason)}&secret=${CONFIG.BOT_SECRET}`
      );
      if (!result?.ok) return interaction.editReply(`❌ Ban failed: ${result?.error ?? "unknown error"}`);

      const username  = result.username || `User#${gameUID}`;
      const banUntilTs = result.ban_until ?? 0;
      const expiresStr = banUntilTs ? `<t:${banUntilTs}:F> (<t:${banUntilTs}:R>)` : "Never (permanent)";

      // DM the player
      let dmStatus = "No linked Discord — DM not sent.";
      const linkedDiscordID = discordID ?? result.discord_id ?? null;
      if (linkedDiscordID) {
        const dmEmbed = new EmbedBuilder()
          .setColor(0xe74c3c)
          .setTitle("🔨 You Have Been Banned")
          .setDescription(
            `Your Dragon Land account **${username}** (\`${gameUID}\`) has been **banned**.\n\n` +
            `⏳ **Duration:** ${durationLabel}\n` +
            `📅 **Expires:** ${expiresStr}\n` +
            `📝 **Reason:** ${reason}\n\n` +
            `Your account data is **not deleted** — the ban will lift automatically when it expires.\n` +
            `You will not appear on leaderboards during this period.\n\n` +
            `If you believe this is a mistake, contact a server admin.`
          )
          .setTimestamp();
        dmStatus = await tryDM(linkedDiscordID, dmEmbed)
          ? `DM sent to <@${linkedDiscordID}>`
          : `Could not DM <@${linkedDiscordID}> (DMs closed or user left).`;
      }

      return interaction.editReply({
        embeds: [new EmbedBuilder()
          .setColor(0xe74c3c)
          .setTitle("🔨 Player Banned")
          .addFields(
            { name: "👤 Player",    value: `**${username}** \`${gameUID}\``,  inline: true },
            { name: "⏳ Duration",  value: durationLabel,                     inline: true },
            { name: "📅 Expires",   value: expiresStr,                        inline: false },
            { name: "📝 Reason",    value: reason,                            inline: false },
            { name: "📬 DM Status", value: dmStatus,                          inline: false },
          )
          .setFooter({ text: "Account preserved · Hidden from leaderboards · Cannot migrate" })
          .setTimestamp()],
      });
    } catch (err) {
      console.error("/ban error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /unban ────────────────────────────────────────────────
  if (commandName === "unban") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    const target = await resolveTarget(interaction);
    if (!target) return;
    const { gameUID, discordID } = target;

    try {
      const result = await apiPost(`unban?user_id=${gameUID}&secret=${CONFIG.BOT_SECRET}`);
      if (!result?.ok) return interaction.editReply(`❌ Unban failed: ${result?.error ?? "unknown error"}`);

      const username = result.username || `User#${gameUID}`;

      // DM the player
      let dmStatus = "No linked Discord — DM not sent.";
      const linkedDiscordID = discordID ?? result.discord_id ?? null;
      if (linkedDiscordID) {
        const dmEmbed = new EmbedBuilder()
          .setColor(0x2ecc71)
          .setTitle("✅ Your Ban Has Been Lifted")
          .setDescription(
            `Your Dragon Land account **${username}** (\`${gameUID}\`) has been **unbanned**.\n\n` +
            `You can now play normally and will reappear on the leaderboards. Welcome back! 🐉`
          )
          .setTimestamp();
        dmStatus = await tryDM(linkedDiscordID, dmEmbed)
          ? `DM sent to <@${linkedDiscordID}>`
          : `Could not DM <@${linkedDiscordID}> (DMs closed or user left).`;
      }

      return interaction.editReply({
        embeds: [new EmbedBuilder()
          .setColor(0x2ecc71)
          .setTitle("✅ Player Unbanned")
          .addFields(
            { name: "👤 Player",    value: `**${username}** \`${gameUID}\``, inline: true },
            { name: "📬 DM Status", value: dmStatus,                        inline: false },
          )
          .setFooter({ text: "Player can now play and appear on leaderboards again" })
          .setTimestamp()],
      });
    } catch (err) {
      console.error("/unban error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /wipe (permanent data deletion — what /ban used to do) ─
  if (commandName === "wipe") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Only the bot owner or admins can use this command.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    const target = await resolveTarget(interaction);
    if (!target) return;
    const { gameUID, discordID } = target;

    const reason = interaction.options.getString("reason") ?? "No reason provided.";

    // Confirmation step
    const row = new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`wipe_confirm:${interaction.user.id}:${gameUID}`)
        .setLabel("💥 Yes, permanently wipe all data")
        .setStyle(ButtonStyle.Danger),
      new ButtonBuilder()
        .setCustomId(`wipe_cancel:${interaction.user.id}`)
        .setLabel("Cancel")
        .setStyle(ButtonStyle.Secondary),
    );

    const confirmMsg = await interaction.editReply({
      embeds: [new EmbedBuilder()
        .setColor(0xe74c3c)
        .setTitle("⚠️ Confirm Data Wipe")
        .setDescription(
          `You are about to **permanently delete ALL data** for player \`${gameUID}\`.\n\n` +
          `This includes their progress, coins, gems, leaderboard scores, and Discord link.\n\n` +
          `📝 **Reason:** ${reason}\n\n` +
          `**This cannot be undone.**`
        )
        .setTimestamp()],
      components: [row],
    });

    try {
      const confirm = await confirmMsg.awaitMessageComponent({
        filter: (i) => i.user.id === interaction.user.id,
        componentType: ComponentType.Button,
        time: 30_000,
      });

      if (confirm.customId.startsWith("wipe_cancel")) {
        return confirm.update({ content: "❌ Wipe cancelled.", embeds: [], components: [] });
      }

      await confirm.deferUpdate();
      const result = await banPlayer(gameUID); // calls the original ban endpoint (full data wipe)
      if (!result?.ok) {
        return confirm.editReply({ content: `❌ Wipe failed: ${result?.error ?? "unknown error"}`, embeds: [], components: [] });
      }

      const username = result.username || `User#${gameUID}`;

      // DM the player before their link is removed
      let dmStatus = "No linked Discord — DM not sent.";
      const linkedDiscordID = discordID ?? result.discord_id ?? null;
      if (linkedDiscordID) {
        const dmEmbed = new EmbedBuilder()
          .setColor(0xe74c3c)
          .setTitle("💥 Your Account Has Been Deleted")
          .setDescription(
            `Your Dragon Land account **${username}** (\`${gameUID}\`) has been **permanently deleted** by a server admin.\n\n` +
            `📝 **Reason:** ${reason}\n\n` +
            `All your data — progress, coins, gems, leaderboard scores — has been wiped.\n` +
            `Your Discord link has been removed.\n\n` +
            `If you believe this is a mistake, contact a server admin.`
          )
          .setTimestamp();
        dmStatus = await tryDM(linkedDiscordID, dmEmbed)
          ? `DM sent to <@${linkedDiscordID}>`
          : `Could not DM <@${linkedDiscordID}> (DMs closed or user left).`;
      }

      return confirm.editReply({
        embeds: [new EmbedBuilder()
          .setColor(0xe74c3c)
          .setTitle("💥 Player Data Wiped")
          .addFields(
            { name: "👤 Player",    value: `**${username}** \`${gameUID}\``, inline: true },
            { name: "📝 Reason",    value: reason,                           inline: false },
            { name: "📬 DM Status", value: dmStatus,                         inline: false },
          )
          .setFooter({ text: "All data deleted · Discord link removed · Sessions invalidated" })
          .setTimestamp()],
        components: [],
      });
    } catch {
      return interaction.editReply({ content: "⏱️ Timed out — no action taken.", embeds: [], components: [] });
    }
  }

  // ── /unlink ───────────────────────────────────────────────
  if (commandName === "unlink") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    const target = await resolveTarget(interaction);
    if (!target) return;
    const { gameUID, discordID } = target;

    try {
      const result = await apiPost(`unlink?user_id=${gameUID}&secret=${CONFIG.BOT_SECRET}`);
      if (!result?.ok) return interaction.editReply(`❌ Unlink failed: ${result?.error ?? "unknown error"}`);

      const username = result.username || `User#${gameUID}`;

      // DM the player
      let dmStatus = "No linked Discord — DM not sent.";
      const linkedDiscordID = discordID ?? result.discord_id ?? null;
      if (linkedDiscordID) {
        const dmEmbed = new EmbedBuilder()
          .setColor(0xe67e22)
          .setTitle("🔗 Your Discord Has Been Unlinked")
          .setDescription(
            `Your Discord account has been **unlinked** from Dragon Land account **${username}** (\`${gameUID}\`) by a server admin.\n\n` +
            `Your game data is **not deleted** — only the Discord link was removed.\n\n` +
            `You can re-link a different account using \`/register <user_id>\` if needed.`
          )
          .setTimestamp();
        dmStatus = await tryDM(linkedDiscordID, dmEmbed)
          ? `DM sent to <@${linkedDiscordID}>`
          : `Could not DM <@${linkedDiscordID}> (DMs closed or user left).`;
      }

      return interaction.editReply({
        embeds: [new EmbedBuilder()
          .setColor(0xe67e22)
          .setTitle("🔗 Discord Unlinked")
          .setDescription(`Game account \`${gameUID}\` (**${username}**) has been unlinked from its Discord.`)
          .addFields({ name: "📬 DM Status", value: dmStatus })
          .setFooter({ text: "Game data is intact · Discord link removed" })
          .setTimestamp()],
      });
    } catch (err) {
      console.error("/unlink error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /whitelist ────────────────────────────────────────────
  if (commandName === "whitelist") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });
    let uid = interaction.options.getString("userid")?.trim();
    const wlMention   = interaction.options.getUser("user");
    const wlDiscordID = interaction.options.getString("discord_id")?.trim();
    if (wlMention) {
      const linked = await apiGet(`is_registered?discord_id=${wlMention.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
      if (!linked?.ok || !linked.registered) return interaction.editReply(`❌ <@${wlMention.id}> has no linked game account.`);
      uid = String(linked.game_user_id);
    } else if (!uid && wlDiscordID) {
      const resolved = await resolveDiscordID(wlDiscordID);
      if (resolved.error) return interaction.editReply(resolved.error);
      uid = resolved.gameUID;
    }
    const action = interaction.options.getString("action");
    const note   = interaction.options.getString("note") ?? "";
    if (!uid || !/^\d+$/.test(uid)) return interaction.editReply("❌ Provide a player game ID or @mention.");
    try {
      const r = await apiPost(`whitelist?user_id=${uid}&action=${action}&note=${encodeURIComponent(note)}&secret=${CONFIG.BOT_SECRET}`);
      if (!r?.ok) return interaction.editReply(`❌ Failed: ${r?.error ?? "unknown"}`);
      return interaction.editReply(
        action === "add"
          ? `✅ Player \`${uid}\` **whitelisted** — cheat detection disabled for them.`
          : `✅ Player \`${uid}\` removed from whitelist — cheat detection re-enabled.`
      );
    } catch { return interaction.editReply("❌ Server error."); }
  }

  // ── /blacklist ────────────────────────────────────────────
  if (commandName === "blacklist") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });
    const deviceUID = interaction.options.getString("device_uid").trim();
    const action    = interaction.options.getString("action");
    const reason    = interaction.options.getString("reason") ?? "";
    const playerID  = interaction.options.getString("player_id")?.trim() ?? null;

    if (!deviceUID) return interaction.editReply("❌ Device UID cannot be empty.");
    try {
      const r = await apiPost(
        `blacklist?device_uid=${encodeURIComponent(deviceUID)}&action=${action}&reason=${encodeURIComponent(reason)}&secret=${CONFIG.BOT_SECRET}`
      );
      if (!r?.ok) return interaction.editReply(`❌ Failed: ${r?.error ?? r ?? "unknown"}`);

      // DM the player if a player ID was provided
      let dmStatus = playerID ? "Attempting to DM..." : "No player ID provided — DM skipped.";
      if (playerID && /^\d+$/.test(playerID)) {
        try {
          const linked = await apiGet(`linked_account_by_game_id?user_id=${playerID}&secret=${CONFIG.BOT_SECRET}`);
          if (linked?.linked && linked?.discord_id) {
            const dmEmbed = new EmbedBuilder()
              .setColor(action === "add" ? 0xe74c3c : 0x2ecc71)
              .setTitle(action === "add" ? "🚫 Your Device Has Been Banned" : "✅ Your Device Ban Has Been Lifted")
              .setDescription(
                action === "add"
                  ? `A device associated with your Dragon Land account has been **banned**.\n\n` +
                    `📱 **Device UID:** \`${deviceUID}\`\n` +
                    `📝 **Reason:** ${reason || "No reason provided."}\n\n` +
                    `If this is a mistake, please contact a server admin.`
                  : `A device associated with your Dragon Land account has been **unbanned**.\n\n` +
                    `📱 **Device UID:** \`${deviceUID}\`\n\n` +
                    `You can now log in from this device again.`
              )
              .setTimestamp();
            dmStatus = await tryDM(linked.discord_id, dmEmbed)
              ? `DM sent to <@${linked.discord_id}>`
              : `Could not DM <@${linked.discord_id}> (DMs closed or user left).`;
          } else {
            dmStatus = "Player has no linked Discord — DM not sent.";
          }
        } catch {
          dmStatus = "Could not look up player's Discord.";
        }
      }

      return interaction.editReply({
        embeds: [new EmbedBuilder()
          .setColor(action === "add" ? 0xe74c3c : 0x2ecc71)
          .setTitle(action === "add" ? "🚫 Device Banned" : "✅ Device Unbanned")
          .addFields(
            { name: "📱 Device UID", value: `\`${deviceUID}\``,              inline: false },
            { name: "📝 Reason",     value: reason || "None",                inline: true  },
            { name: "📬 DM Status",  value: dmStatus,                        inline: false },
          )
          .setTimestamp()],
      });
    } catch { return interaction.editReply("❌ Server error."); }
  }

  // ── /devices ──────────────────────────────────────────────
  if (commandName === "devices") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });
    let uid = interaction.options.getString("userid")?.trim();
    const devMention   = interaction.options.getUser("user");
    const devDiscordID = interaction.options.getString("discord_id")?.trim();
    if (devMention) {
      const linked = await apiGet(`is_registered?discord_id=${devMention.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
      if (!linked?.ok || !linked.registered) return interaction.editReply(`❌ <@${devMention.id}> has no linked game account.`);
      uid = String(linked.game_user_id);
    } else if (!uid && devDiscordID) {
      const resolved = await resolveDiscordID(devDiscordID);
      if (resolved.error) return interaction.editReply(resolved.error);
      uid = resolved.gameUID;
    }
    if (!uid || !/^\d+$/.test(uid)) return interaction.editReply("❌ Provide a player game ID or @mention.");
    try {
      const r = await apiGet(`devices?user_id=${uid}&secret=${CONFIG.BOT_SECRET}`);
      if (!r?.ok) return interaction.editReply(`❌ ${r?.error ?? "Unknown error."}`);
      if (!r.devices?.length) return interaction.editReply(`ℹ️ No devices found for player \`${uid}\`.`);

      const lines = r.devices.map((d) => {
        const banned = d.banned ? " 🚫 **BANNED**" : "";
        const seen   = new Date(d.last_seen * 1000).toISOString().slice(0, 10);
        return `\`${d.device_uid}\`${banned}\n> ${d.device_model || "Unknown"} · ${d.device_os || "?"} · ${d.platform || "?"} · Last seen: ${seen}`;
      }).join("\n\n");

      const embed = new EmbedBuilder()
        .setColor(0x95a5a6)
        .setTitle(`📱 Devices for Player \`${uid}\``)
        .setDescription(lines)
        .setFooter({ text: "Use /blacklist <device_uid> add player_id:<game_id> to ban a device and notify the player" })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch { return interaction.editReply("❌ Server error."); }
  }

  // ── /registered ───────────────────────────────────────────
  if (commandName === "registered") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });
    const page = interaction.options.getInteger("page") ?? 1;
    try {
      const r = await apiGet(`registered?page=${page}&secret=${CONFIG.BOT_SECRET}`);
      if (!r?.ok) return interaction.editReply(`❌ ${r?.error ?? "Unknown error."}`);

      const accounts = r.accounts ?? [];
      if (!accounts.length) return interaction.editReply(`📭 No linked accounts on page ${page}.`);

      const lines = accounts.map((a) => {
        const name    = a.game_username?.trim() || `User#${a.game_user_id}`;
        const date    = new Date(a.linked_at * 1000).toISOString().slice(0, 10);
        return `<@${a.discord_id}> (ID: \`${a.discord_id}\`)\n> 🎮 **${name}** · Game ID \`${a.game_user_id}\` · Linked ${date}`;
      }).join("\n\n");

      const totalPages = Math.ceil(r.total / r.per_page);
      const embed = new EmbedBuilder()
        .setColor(0x2ecc71)
        .setTitle(`🔗 Registered Accounts — Page ${page}/${totalPages}`)
        .setDescription(lines)
        .setFooter({ text: `${r.total} total linked accounts · /registered page:${page + 1} for next page` })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch { return interaction.editReply("❌ Server error."); }
  }

  // ── /isregistered ─────────────────────────────────────────
  if (commandName === "isregistered") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Admin only.", ephemeral: true });
    }
    const mentionedUser = interaction.options.getUser("user");
    let discordID = mentionedUser?.id ?? interaction.options.getString("discord_id")?.trim();
    if (!discordID) {
      return interaction.reply({ content: "❌ Provide a Discord @mention or ID.", ephemeral: true });
    }
    const mentionMatch = discordID.match(/^<@!?(\d+)>$/);
    if (mentionMatch) discordID = mentionMatch[1];
    if (!/^\d+$/.test(discordID)) {
      return interaction.reply({ content: "❌ Invalid Discord ID.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    try {
      const r = await apiGet(`is_registered?discord_id=${discordID}&secret=${CONFIG.BOT_SECRET}`);
      if (!r?.ok) return interaction.editReply(`❌ ${r?.error ?? "Server error."}`);

      if (!r.registered) {
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0x95a5a6)
            .setTitle("🔍 Not Registered")
            .setDescription(`Discord ID \`${discordID}\` has **not** linked any game account.`)
            .setTimestamp()],
        });
      }

      return interaction.editReply({
        embeds: [new EmbedBuilder()
          .setColor(0x2ecc71)
          .setTitle("🔍 Registered Account Found")
          .setDescription(`Discord ID \`${discordID}\` is linked to:`)
          .addFields(
            { name: "👤 Username",  value: r.username,              inline: true },
            { name: "🆔 Game ID",   value: `\`${r.game_user_id}\``, inline: true },
            { name: "💰 Coins",     value: fmt(r.coins),            inline: true },
            { name: "💎 Gems",      value: fmt(r.gems),             inline: true },
            { name: "🏆 Max Level", value: String(r.max_level),     inline: true },
          )
          .setFooter({ text: `Use /ban ${r.game_user_id} to soft-ban · /wipe ${r.game_user_id} to delete all data` })
          .setTimestamp()],
      });
    } catch (err) {
      console.error("/isregistered error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /register ─────────────────────────────────────────────
  if (commandName === "register") {
    const uid = interaction.options.getString("userid").trim();
    if (!/^\d+$/.test(uid)) {
      return interaction.reply({ content: "❌ User ID must be numbers only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    try {
      const r = await apiPost(
        `generate_code?user_id=${uid}&discord_id=${interaction.user.id}&secret=${CONFIG.BOT_SECRET}`
      );

      if (!r?.ok) {
        if (r?.error === "discord_already_linked") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe67e22)
              .setTitle("⚠️ Already Linked")
              .setDescription(
                `Your Discord is already linked to game account \`${r.linked_user_id}\`.\n\n` +
                `**Got a new user ID after reinstalling?**\n` +
                `Use \`/login ${uid}\` to migrate all your progress to the new account.`
              )
              .setTimestamp()],
          });
        }
        if (r?.error === "already_linked") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe74c3c)
              .setTitle("❌ Account Already Claimed")
              .setDescription(
                `Game account \`${uid}\` is already linked to a different Discord user.\n\n` +
                `If this is your account, please contact a server admin.`
              )
              .setTimestamp()],
          });
        }
        if (r?.error === "rate_limited") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe67e22)
              .setTitle("⏳ Too Many Attempts")
              .setDescription("You've tried too many times. Please wait an hour before trying again.")
              .setTimestamp()],
          });
        }
        if (r?.error === "user_not_found") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe74c3c)
              .setTitle("❌ Account Not Found")
              .setDescription(
                `Game account \`${uid}\` doesn't exist on the server yet.\n\n` +
                `Make sure you're entering the right user ID, then **complete at least the first level** — your account won't be registered until you do.\n\n` +
                `After finishing a level, run \`/register ${uid}\` again.`
              )
              .setTimestamp()],
          });
        }
        if (r?.error === "no_progress") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe67e22)
              .setTitle("🎮 Complete a Level First")
              .setDescription(
                `You need to **finish at least one level** in Dragon Land before linking your account.\n\n` +
                `Play through the first stage, then run \`/register ${uid}\` again!`
              )
              .setTimestamp()],
          });
        }
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("❌ Error")
            .setDescription(r?.msg ?? r?.error ?? "Something went wrong. Try again later.")
            .setTimestamp()],
        });
      }

      try {
        await interaction.user.send({
          embeds: [new EmbedBuilder()
            .setColor(0x3498db)
            .setTitle("🔐 Dragon Land — Verify Your Account")
            .setDescription(
              `You're linking your Discord to game account \`${uid}\`.\n\n` +
              `**Follow these steps:**\n` +
              `1️⃣ Open Dragon Land **right now** (the game just forced a reconnect)\n` +
              `2️⃣ Look at your **lives counter** — the hearts at the top of the screen\n` +
              `3️⃣ You'll see a **6-digit number** where your hearts normally are\n` +
              `4️⃣ **Reply to this DM** with that 6-digit number\n\n` +
              `⏱️ You have **5 minutes** — after that the code expires and your lives return to normal.\n\n` +
              `⚠️ **Never share this code with anyone**, not even admins.`
            )
            .setFooter({ text: "The code disappears automatically after 5 minutes." })
            .setTimestamp()],
        });
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0x2ecc71)
            .setTitle("📬 Check Your DMs!")
            .setDescription(
              `Open Dragon Land and look at your **lives counter** — it now shows a 6-digit code.\n\n` +
              `Reply to the bot DM with that number to confirm your link.\n\n` +
              `⏱️ The code expires in **5 minutes**.`
            )
            .setTimestamp()],
        });
      } catch {
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe67e22)
            .setTitle("⚠️ Couldn't Send You a DM")
            .setDescription(
              `Please enable DMs from server members:\n` +
              `*Right-click the server → Privacy Settings → Allow direct messages from server members*\n\n` +
              `Then run \`/register ${uid}\` again.\n\n` +
              `**In the meantime:** open Dragon Land — your **lives counter** is now showing a 6-digit code. DM it to me within 5 minutes.`
            )
            .setTimestamp()],
        });
      }
    } catch (err) {
      console.error("/register error:", err);
      return interaction.editReply("❌ Server error. Try again in a moment.");
    }
  }

  // ── /gift ─────────────────────────────────────────────────
  if (commandName === "gift") {
    if (!isAdmin(interaction)) {
      return interaction.reply({ content: "❌ Only the bot owner or admins can use this command.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    const giftType      = interaction.options.getString("type");
    const amount        = interaction.options.getInteger("amount");
    const mentionedUser = interaction.options.getUser("user");
    const giftDiscordID = interaction.options.getString("discord_id")?.trim();
    let targetGameID    = interaction.options.getString("userid").trim();
    let targetDiscordID = null;

    if (mentionedUser) {
      const linked = await apiGet(`is_registered?discord_id=${mentionedUser.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
      if (!linked?.ok || !linked.registered)
        return interaction.editReply(`❌ <@${mentionedUser.id}> has no linked game account.`);
      targetGameID    = String(linked.game_user_id);
      targetDiscordID = mentionedUser.id;
    } else if (giftDiscordID) {
      const resolved = await resolveDiscordID(giftDiscordID);
      if (resolved.error) return interaction.editReply(resolved.error);
      targetGameID    = resolved.gameUID;
      targetDiscordID = giftDiscordID;
    }

    if (!/^\d+$/.test(targetGameID))
      return interaction.editReply("❌ Invalid game ID — must be numbers only.");

    try {
      const r = await apiPost(`gift?user_id=${targetGameID}&type=${giftType}&amount=${amount}&secret=${CONFIG.BOT_SECRET}`);
      if (!r?.ok) return interaction.editReply(`❌ Gift failed: ${r?.error ?? "unknown error"}`);

      const emojiMap  = { coins: "💰", gems: "💎", lives: "❤️" };
      const labelMap  = { coins: "Coins", gems: "Gems", lives: "Lives" };
      const colorMap  = { coins: 0xf1c40f, gems: 0x9b59b6, lives: 0xe74c3c };

      // DM the recipient
      let dmStatus = "No linked Discord — DM not sent.";
      const recipientDiscordID = targetDiscordID ?? r.discord_id ?? null;
      if (!recipientDiscordID) {
        // Try to look it up from the game ID
        try {
          const linked = await apiGet(`linked_account_by_game_id?user_id=${targetGameID}&secret=${CONFIG.BOT_SECRET}`);
          if (linked?.linked && linked?.discord_id) {
            const dmEmbed = new EmbedBuilder()
              .setColor(colorMap[giftType] ?? 0x2ecc71)
              .setTitle(`${emojiMap[giftType]} You Received a Gift!`)
              .setDescription(
                `A server admin has gifted you **${fmt(r.gifted)} ${labelMap[giftType]}** in Dragon Land!\n\n` +
                `💼 **Before:** ${fmt(r.before)}  →  **After:** ${fmt(r.after)}\n\n` +
                `The gift will appear on your next game launch. 🐉`
              )
              .setTimestamp();
            dmStatus = await tryDM(linked.discord_id, dmEmbed)
              ? `DM sent to <@${linked.discord_id}>`
              : `Could not DM <@${linked.discord_id}> (DMs closed or user left).`;
          } else {
            dmStatus = "No linked Discord — DM not sent.";
          }
        } catch { dmStatus = "Could not look up player's Discord."; }
      } else {
        const dmEmbed = new EmbedBuilder()
          .setColor(colorMap[giftType] ?? 0x2ecc71)
          .setTitle(`${emojiMap[giftType]} You Received a Gift!`)
          .setDescription(
            `A server admin has gifted you **${fmt(r.gifted)} ${labelMap[giftType]}** in Dragon Land!\n\n` +
            `💼 **Before:** ${fmt(r.before)}  →  **After:** ${fmt(r.after)}\n\n` +
            `The gift will appear on your next game launch. 🐉`
          )
          .setTimestamp();
        dmStatus = await tryDM(recipientDiscordID, dmEmbed)
          ? `DM sent to <@${recipientDiscordID}>`
          : `Could not DM <@${recipientDiscordID}> (DMs closed or user left).`;
      }

      const embed = new EmbedBuilder()
        .setColor(colorMap[giftType] ?? 0x2ecc71)
        .setTitle(`${emojiMap[giftType]} Gift Sent!`)
        .setDescription(`**${r.username}** (\`${targetGameID}\`) received **${fmt(r.gifted)} ${labelMap[giftType]}**.`)
        .addFields(
          { name: "Before",       value: fmt(r.before),   inline: true },
          { name: "Gifted",       value: `+${fmt(r.gifted)}`, inline: true },
          { name: "After",        value: fmt(r.after),    inline: true },
          { name: "📬 DM Status", value: dmStatus,        inline: false },
        )
        .setFooter({ text: "Player will see the gift on next game launch." })
        .setTimestamp();
      return interaction.editReply({ embeds: [embed] });
    } catch (err) {
      console.error("/gift error:", err);
      return interaction.editReply("❌ Server error.");
    }
  }

  // ── /ftrank ───────────────────────────────────────────────
  if (commandName === "ftrank") {
    await interaction.deferReply({ ephemeral: false });

    try {
      let targetUID = interaction.options.getString("userid")?.trim();
      const ftMention   = interaction.options.getUser("user");
      const ftDiscordID = interaction.options.getString("discord_id")?.trim();
      let isOwnProfile = false;

      if (ftMention) {
        const linked = await apiGet(`is_registered?discord_id=${ftMention.id}&secret=${CONFIG.BOT_SECRET}`).catch(() => null);
        if (!linked?.ok || !linked.registered) return interaction.editReply(`❌ <@${ftMention.id}> has no linked game account.`);
        targetUID = String(linked.game_user_id);
      } else if (!targetUID && ftDiscordID) {
        const resolved = await resolveDiscordID(ftDiscordID);
        if (resolved.error) return interaction.editReply(resolved.error);
        targetUID = resolved.gameUID;
      } else if (targetUID && !/^\d+$/.test(targetUID)) {
        return interaction.editReply("❌ User ID must be numbers only.");
      } else if (targetUID) {
        const lookup = await apiGet(`search_player?user_id=${targetUID}`).catch(() => null);
        if (!lookup?.results?.length) return interaction.editReply(`❌ Player \`${targetUID}\` doesn't exist on the server.`);
      }

      if (!targetUID) {
        const linked = await apiGet(`linked_account?discord_id=${interaction.user.id}&secret=${CONFIG.BOT_SECRET}`);
        if (!linked?.linked) {
          return interaction.editReply({ content: "❌ You don't have a linked account yet. Use `/register` first." });
        }
        targetUID = String(linked.user_id);
        isOwnProfile = true;
      }

      const ftLb = await fetchLeaderboard("FAST_TRACK").catch(() => null);
      const entry = ftLb?.ok ? ftLb.ranking?.find((e) => String(e.user_id) === targetUID) : null;
      const score = entry ? Number(entry.score) : 0;

      const currentTier = ftTierForScore(score);
      const guild = interaction.guild;

      const tierLines = CONFIG.FT_RANKS.map((t) => {
        const reached   = score >= t.minScore;
        const isCurrent = currentTier?.key === t.key;
        const emojiStr  = ftRankEmoji(t.key, guild);
        const mark      = isCurrent ? "▶ " : "  ";
        return `${mark}${reached ? "✅" : "⬜"} ${emojiStr} **${t.label}** — ${Number(t.minScore).toLocaleString()} pts`;
      }).join("\n");

      const nextTier = CONFIG.FT_RANKS.find((t) => score < t.minScore);
      let progressLine = "";
      if (nextTier) {
        const needed = nextTier.minScore - score;
        const pct    = Math.min(100, Math.round((score / nextTier.minScore) * 100));
        const filled = Math.round(pct / 10);
        const bar    = "█".repeat(filled) + "░".repeat(10 - filled);
        progressLine = `\n\n**Next rank:** ${ftRankEmoji(nextTier.key, guild)} ${nextTier.label}\n\`${bar}\` ${pct}% — **${Number(needed).toLocaleString()}** more pts needed`;
      } else {
        progressLine = "\n\n🏆 **Max rank reached!**";
      }

      const playerName = entry?.name?.trim() || `User#${targetUID}`;
      const rankLabel  = currentTier ? `${ftRankEmoji(currentTier.key, guild)} ${currentTier.label}` : "Unranked";
      const rankLbLine = entry ? ` · Leaderboard rank **#${entry.rank}**` : "";

      const embed = new EmbedBuilder()
        .setColor(currentTier?.color ?? 0x99aab5)
        .setTitle(`🚀 Fast Track Rank — ${playerName}`)
        .setDescription(
          `**Current rank:** ${rankLabel}${rankLbLine}\n🪙 Score: **${Number(score).toLocaleString()} coins**` +
          progressLine
        )
        .addFields({ name: "All Tiers", value: tierLines })
        .setFooter({ text: isOwnProfile ? "Your Fast Track rank · Use /leaderboard type:Fast Track for the full board" : `Game ID: ${targetUID}` })
        .setTimestamp();

      return interaction.editReply({ embeds: [embed] });
    } catch (err) {
      console.error("/ftrank error:", err);
      return interaction.editReply("❌ Server error. Try again in a moment.");
    }
  }

  // ── /login (account migration) ────────────────────────────
  if (commandName === "login") {
    const newUID = interaction.options.getString("newuserid").trim();
    if (!/^\d+$/.test(newUID)) {
      return interaction.reply({ content: "❌ User ID must be numbers only.", ephemeral: true });
    }
    await interaction.deferReply({ ephemeral: true });

    try {
      // ── 1. Must already have a linked account to migrate from ──
      const linked = await apiGet(`linked_account?discord_id=${interaction.user.id}&secret=${CONFIG.BOT_SECRET}`);
      if (!linked?.linked) {
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("❌ No Linked Account")
            .setDescription(
              `Your Discord isn't linked to any account yet.\n\n` +
              `Use \`/register <your_user_id>\` first to link your account.`
            ).setTimestamp()],
        });
      }

      const oldUID = String(linked.user_id);
      if (oldUID === newUID) {
        return interaction.editReply(`⚠️ That's already your linked account (\`${newUID}\`). Nothing to do.`);
      }

      // ── 2. Block migration if the current account is banned ──
      const currentStats = await fetchPlayerStats(oldUID).catch(() => null);
      if (currentStats?.banned) {
        const banUntilTs = currentStats.ban_until ?? 0;
        const expiresStr = banUntilTs ? `<t:${banUntilTs}:R>` : "permanently";
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("🔨 Account Migration Blocked")
            .setDescription(
              `Your account \`${oldUID}\` is currently **banned** and cannot be migrated.\n\n` +
              `⏳ Ban expires: ${expiresStr}\n\n` +
              `You can only migrate after your ban is lifted. Contact a server admin if you believe this is a mistake.`
            ).setTimestamp()],
        });
      }

      // ── 3. Check the new account actually exists ──────────────
      let newUserExists = false;
      try {
        const lookup = await apiGet(`search_player?user_id=${newUID}`);
        newUserExists = lookup?.ok && lookup?.results?.length > 0;
      } catch (_) {}

      if (!newUserExists) {
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("❌ Account Not Found")
            .setDescription(
              `Game account \`${newUID}\` doesn't exist on the server yet.\n\n` +
              `**Complete at least the first level** in Dragon Land on your new install, then run \`/login ${newUID}\` again.`
            ).setTimestamp()],
        });
      }

      // ── 4. Check the new UID isn't already owned by someone else ──
      try {
        const newLinked = await apiGet(`linked_account_by_game_id?user_id=${newUID}&secret=${CONFIG.BOT_SECRET}`);
        if (newLinked?.linked && newLinked.discord_id && newLinked.discord_id !== interaction.user.id) {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe74c3c)
              .setTitle("❌ Account Already Claimed")
              .setDescription(
                `Game account \`${newUID}\` is already linked to a different Discord user.\n\n` +
                `You can only migrate to an account you own. If you believe this is a mistake, contact a server admin.`
              ).setTimestamp()],
          });
        }
      } catch (_) { /* no link on newUID is fine — proceed */ }

      // ── 5. Prove ownership of the new UID via in-game code ───
      //  generate_code also invalidates sessions and disables sync on the
      //  new account (same as /register), preventing anyone from sneaking
      //  progress in while the code is pending.
      const codeResp = await apiPost(
        `generate_code?user_id=${newUID}&discord_id=${interaction.user.id}&secret=${CONFIG.BOT_SECRET}&migration=true`
      );

      if (!codeResp?.ok) {
        if (codeResp?.error === "already_linked" || codeResp?.error === "target_already_registered") {
          // The target UID is already registered to a different Discord user
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe74c3c)
              .setTitle("❌ Account Already Registered")
              .setDescription(
                `Game account \`${newUID}\` is already registered to a different Discord user and cannot be migrated to.\n\n` +
                `If you believe this is your account, contact a server admin.`
              ).setTimestamp()],
          });
        }
        if (codeResp?.error === "already_linked") {
          // Double-safety: server says that UID is linked to a different Discord
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe74c3c)
              .setTitle("❌ Account Already Claimed")
              .setDescription(
                `Game account \`${newUID}\` is already linked to a different Discord user.\n\n` +
                `If this is your account, please contact a server admin.`
              ).setTimestamp()],
          });
        }
        if (codeResp?.error === "rate_limited") {
          return interaction.editReply({
            embeds: [new EmbedBuilder()
              .setColor(0xe67e22)
              .setTitle("⏳ Too Many Attempts")
              .setDescription("You've tried too many times. Please wait an hour before trying again.")
              .setTimestamp()],
          });
        }
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe74c3c)
            .setTitle("❌ Error")
            .setDescription(codeResp?.msg ?? codeResp?.error ?? "Could not start verification. Try again later.")
            .setTimestamp()],
        });
      }

      // Store the pending migration so the DM listener can complete it
      pendingLogin.set(interaction.user.id, {
        newUID,
        oldUID,
        linkedName: linked.name ?? null,
        expiresAt: Date.now() + 5 * 60 * 1000, // 5 minutes, same as code TTL
      });

      // Send the same DM verification flow as /register
      try {
        await interaction.user.send({
          embeds: [new EmbedBuilder()
            .setColor(0xe67e22)
            .setTitle("🔐 Dragon Land — Verify Account Ownership")
            .setDescription(
              `You're migrating your account to new game ID \`${newUID}\`.\n\n` +
              `**First, prove you own \`${newUID}\`:**\n` +
              `1️⃣ Open Dragon Land **right now** (the game just forced a reconnect on that account)\n` +
              `2️⃣ Look at your **lives counter** — the hearts at the top of the screen\n` +
              `3️⃣ You'll see a **6-digit number** where your hearts normally are\n` +
              `4️⃣ **Reply to this DM** with that 6-digit number\n\n` +
              `⏱️ You have **5 minutes** — after that the code expires and the migration is cancelled.\n\n` +
              `⚠️ **Never share this code with anyone**, not even admins.`
            )
            .setFooter({ text: "Migration will proceed automatically once the code is confirmed." })
            .setTimestamp()],
        });
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0x2ecc71)
            .setTitle("📬 Check Your DMs!")
            .setDescription(
              `Open Dragon Land on the **new device / fresh install** and look at your **lives counter** — it now shows a 6-digit code.\n\n` +
              `Reply to the bot DM with that number to confirm you own \`${newUID}\`.\n\n` +
              `⏱️ The code expires in **5 minutes**.`
            ).setTimestamp()],
        });
      } catch {
        // User has DMs closed — still works if they DM the bot themselves
        return interaction.editReply({
          embeds: [new EmbedBuilder()
            .setColor(0xe67e22)
            .setTitle("⚠️ Couldn't Send You a DM")
            .setDescription(
              `Please enable DMs from server members:\n` +
              `*Right-click the server → Privacy Settings → Allow direct messages from server members*\n\n` +
              `**In the meantime:** open Dragon Land on the new device — your **lives counter** is now showing a 6-digit code. DM it to this bot within 5 minutes to complete the migration.`
            ).setTimestamp()],
        });
      }

    } catch (err) {
      console.error("/login error:", err);
      return interaction.editReply("❌ Server error. Try again later.");
    }
  }
});

// ── Boot ──────────────────────────────────────────────────────
(async () => {
  if (!CONFIG.TOKEN) {
    console.error("❌ DISCORD_BOT_TOKEN missing — set in /opt/dragonland-dlr/.env");
    process.exit(1);
  }
  if (!CONFIG.CLIENT_ID) {
    console.error("❌ DISCORD_CLIENT_ID missing — Sir Blizzy application ID in .env");
    process.exit(1);
  }
  if (!CONFIG.GUILD_ID) {
    console.error("❌ DISCORD_GUILD_ID missing in .env");
    process.exit(1);
  }
  if (!CONFIG.BOT_SECRET) {
    console.error("❌ BOT_SECRET missing in .env");
    process.exit(1);
  }
  console.log(`Game API: ${CONFIG.API_BASE}`);
  await registerCommands();
  await client.login(CONFIG.TOKEN);
})();
