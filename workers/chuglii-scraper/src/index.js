/**
 * Chuglii Scraper - Dedicated Cloudflare Worker
 * 
 * Features:
 * 1. Scheduled Cron Ingestion: Automatically crawls latest MMS & Viral leaks every 4 hours.
 * 2. Multi-Category Support: Indian, Desi, Pakistani, Bangladeshi, etc.
 * 3. High-Performance Edge Extraction: Direct CDN stream URL derivation & metadata normalization.
 * 4. Segregated Storage: Upserts into Supabase `scraper.videos` via RPC without touching DVR tables.
 */

const CORS_HEADERS = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
  "Access-Control-Allow-Headers": "Content-Type, Authorization, apikey",
};

export default {
  async fetch(request, env, ctx) {
    if (request.method === "OPTIONS") {
      return new Response(null, { status: 204, headers: CORS_HEADERS });
    }

    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";

    try {
      // Health / Status
      if (path === "/" || path === "/status" || path === "/health") {
        return jsonResponse({
          service: "chuglii-scraper",
          status: "healthy",
          timestamp: new Date().toISOString(),
          endpoints: {
            "GET /sync": "Trigger scraping (?category=all|pakistani|bangladesi|indian&pages=1-10)",
            "GET /categories": "List supported categories",
            "GET /health": "Health check",
          },
        });
      }

      // Categories
      if (path === "/categories") {
        return jsonResponse({
          categories: ["all", "indian", "desi", "pakistani", "bangladesi"],
        });
      }

      // Sync Trigger
      if (path === "/sync") {
        const category = url.searchParams.get("category") || "all";
        const pages = Math.min(10, Math.max(1, parseInt(url.searchParams.get("pages") || "2", 10)));

        let result;
        if (category === "all") {
          result = await syncGlobalFeed(env, pages);
        } else {
          result = await syncCategory(env, category, pages);
        }

        return jsonResponse(result);
      }

      return jsonResponse({ error: "Not found", path }, 404);
    } catch (err) {
      return jsonResponse({ error: err.message || "Internal Server Error" }, 500);
    }
  },

  // Cloudflare Cron Trigger (Runs every 15 minutes automatically)
  async scheduled(event, env, ctx) {
    // Ingests every new video from the global latest feed + Pakistani category
    ctx.waitUntil(Promise.all([
      syncGlobalFeed(env, 2),
      syncCategory(env, "pakistani", 1),
    ]));
  },
};

// ─── Helper Functions ────────────────────────────────────────────────

function jsonResponse(data, status = 200) {
  return new Response(JSON.stringify(data, null, 2), {
    status,
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      ...CORS_HEADERS,
    },
  });
}

function parseDurationSeconds(str) {
  if (!str) return 0;
  const parts = str.trim().split(":").map(p => parseInt(p, 10));
  if (parts.length === 2) return (parts[0] || 0) * 60 + (parts[1] || 0);
  if (parts.length === 3) return (parts[0] || 0) * 3600 + (parts[1] || 0) * 60 + (parts[2] || 0);
  return 0;
}

function parseViewsNumber(str) {
  if (!str) return 0;
  const s = str.trim().toUpperCase();
  const num = parseFloat(s.replace(/[^0-9.]/g, ""));
  if (isNaN(num)) return 0;
  if (s.includes("M")) return Math.round(num * 1000000);
  if (s.includes("K")) return Math.round(num * 1000);
  return Math.round(num);
}

function extractTagsFromTitle(title) {
  const commonTags = [
    "desi", "bhabhi", "amateur", "teen", "viral", "mms", "village", "aunty",
    "web-series", "hot", "romance", "mallu", "tamil", "telugu", "bengali",
    "bhojpuri", "college", "girlfriend", "call-recording", "leaked", "scandal",
    "pakistani", "karachi", "lahore", "punjabi", "urdu", "bangladeshi"
  ];
  const tLower = (title || "").toLowerCase();
  const tags = new Set();
  for (const tag of commonTags) {
    if (tLower.includes(tag.replace("-", " ")) || tLower.includes(tag)) {
      tags.add(tag);
    }
  }
  if (tags.size === 0) tags.add("desi");
  return Array.from(tags);
}

async function scrapePage(pageNum, category = null) {
  let url;
  if (category && category !== "all" && category !== "indian" && category !== "desi") {
    url = pageNum === 1
      ? `https://mmsvibex.life/category/${encodeURIComponent(category)}/`
      : `https://mmsvibex.life/category/${encodeURIComponent(category)}/page/${pageNum}/`;
  } else {
    url = pageNum === 1
      ? "https://mmsvibex.life/?filter=latest"
      : `https://mmsvibex.life/page/${pageNum}/?filter=latest`;
  }

  const res = await fetch(url, {
    headers: {
      "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
      "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    },
  });

  if (!res.ok) return [];
  const html = await res.text();

  // Order-independent article extractor
  const articleRegex = /<article\b([^>]*)>([\s\S]*?)<\/article>/gi;
  const items = [];
  let match;

  while ((match = articleRegex.exec(html)) !== null) {
    const attrString = match[1];
    const body = match[2];

    const postIdMatch = attrString.match(/data-post-id=["'](\d+)["']/i);
    if (!postIdMatch) continue;
    const postId = postIdMatch[1];

    const trailerMatch = attrString.match(/data-trailer=["']([^"']+)["']/i);
    const trailerUrl = trailerMatch ? trailerMatch[1] : "";

    const mainThumbMatch = attrString.match(/data-main-thumb=["']([^"']+)["']/i);

    // Extract title & link
    const titleMatch = body.match(/class=["']title["'][^>]*>([^<]+)<\//i)
      || body.match(/title=["']([^"']+)["']/i);
    const title = titleMatch ? titleMatch[1].trim() : `MMS Clip #${postId}`;

    const linkMatch = body.match(/href=["'](https?:\/\/[^"']+)["']/i);
    const detailUrl = linkMatch ? linkMatch[1] : `https://mmsvibex.life/video/${postId}/`;

    // Extract thumbnail
    const thumbMatch = body.match(/src=["']([^"']+)["']/i);
    const thumbUrl = (mainThumbMatch && mainThumbMatch[1])
      || (thumbMatch && thumbMatch[1])
      || `https://cdn.mmsvibex.fit/thumbnails/${postId}_thumb_960.jpg`;

    // Extract duration
    const durMatch = body.match(/class=["']duration["'][^>]*>([^<]+)<\//i);
    const durFormatted = durMatch ? durMatch[1].trim() : "00:00";

    // Extract views
    const viewsMatch = body.match(/class=["']views["'][^>]*>([^<]+)<\//i);
    const viewsRaw = viewsMatch ? viewsMatch[1].trim() : "0";

    // Extract rating
    const ratingMatch = body.match(/class=["']rating["'][^>]*>([^<]+)<\//i);
    const ratingRaw = ratingMatch ? ratingMatch[1].trim() : "0%";

    // Fast direct CDN processed stream derivation
    let fullVideoUrl = `https://cdn.mmsvibex.fit/processed/${postId}_final.mp4`;
    if (trailerUrl) {
      const trMatch = trailerUrl.match(/\/trailers\/(\d+)_trailer\.mp4/i);
      if (trMatch) {
        fullVideoUrl = `https://cdn.mmsvibex.fit/processed/${trMatch[1]}_final.mp4`;
      }
    }

    // Parse classes for categories, tags, actors
    const classMatch = attrString.match(/class=["']([^"']+)["']/i);
    const classes = classMatch ? classMatch[1].split(/\s+/) : [];

    const categories = classes
      .filter(c => c.startsWith("category-"))
      .map(c => c.replace("category-", ""));
    
    if (category && category !== "all" && !categories.includes(category)) {
      categories.push(category);
    }
    // All leaks are categorized under 'indian' on chuglii.in, plus their native categories
    if (!categories.includes("indian")) {
      categories.push("indian");
    }
    if (!categories.includes("desi")) {
      categories.push("desi");
    }

    const tags = classes
      .filter(c => c.startsWith("tag-"))
      .map(c => c.replace("tag-", ""));
    if (tags.length === 0) {
      extractTagsFromTitle(title).forEach(t => tags.push(t));
    }

    const actors = classes
      .filter(c => c.startsWith("actors-"))
      .map(c => c.replace("actors-", ""));

    items.push({
      source_site: "mmsvibex",
      source_post_id: postId,
      title,
      slug: title.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 100),
      detail_url: detailUrl,
      full_video_url: fullVideoUrl,
      video_mime_type: "video/mp4",
      trailer_preview_url: trailerUrl || null,
      thumbnail_url: thumbUrl,
      clean_thumbnail_url: thumbUrl,
      duration_seconds: parseDurationSeconds(durFormatted),
      duration_formatted: durFormatted,
      views_count: parseViewsNumber(viewsRaw),
      views_raw: viewsRaw,
      rating_score: parseFloat(ratingRaw) || 0,
      rating_raw: ratingRaw,
      categories,
      tags,
      actors,
      published_at: new Date().toISOString(),
      raw_payload: { post_id: postId, trailer: trailerUrl },
    });
  }

  return items;
}

async function sendBatchToSupabase(env, items) {
  if (!items || items.length === 0) return { count: 0, success: true };

  const supabaseUrl = env.SUPABASE_URL || "https://supabase.chuglii.in";
  const supabaseKey = env.SUPABASE_SERVICE_ROLE_KEY || env.SUPABASE_ANON_KEY;

  const res = await fetch(`${supabaseUrl}/rest/v1/rpc/scraper_upsert_videos`, {
    method: "POST",
    headers: {
      "apikey": supabaseKey,
      "Authorization": `Bearer ${supabaseKey}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ items }),
  });

  if (!res.ok) {
    const errorText = await res.text();
    throw new Error(`Supabase upsert failed [${res.status}]: ${errorText}`);
  }

  return await res.json();
}

async function syncCategory(env, category, maxPages = 2) {
  const allItems = [];
  for (let p = 1; p <= maxPages; p++) {
    try {
      const items = await scrapePage(p, category);
      allItems.push(...items);
    } catch {}
  }

  if (allItems.length === 0) {
    return { success: false, category, items_scraped: 0, message: "No items parsed" };
  }

  const upsertResult = await sendBatchToSupabase(env, allItems);
  return {
    success: true,
    category,
    pages_crawled: maxPages,
    items_scraped: allItems.length,
    upsert_result: upsertResult,
  };
}

async function syncGlobalFeed(env, maxPages = 2) {
  const allItems = [];
  for (let p = 1; p <= maxPages; p++) {
    try {
      // Scrape global latest feed (every newly uploaded clip on the entire site)
      const items = await scrapePage(p, null);
      allItems.push(...items);
    } catch {}
  }

  if (allItems.length === 0) {
    return { success: false, feed: "global_latest", items_scraped: 0, message: "No items parsed" };
  }

  const upsertResult = await sendBatchToSupabase(env, allItems);
  return {
    success: true,
    feed: "global_latest",
    pages_crawled: maxPages,
    items_scraped: allItems.length,
    upsert_result: upsertResult,
  };
}

async function syncAllCategories(env, pagesPerCat = 2) {
  const results = {};
  let totalItems = 0;

  // 1. Indian / Latest
  try {
    const indianRes = await syncCategory(env, "indian", pagesPerCat);
    results.indian = indianRes;
    totalItems += indianRes.items_scraped || 0;
  } catch (err) {
    results.indian = { success: false, error: err.message };
  }

  // 2. Pakistani
  try {
    const pakRes = await syncCategory(env, "pakistani", pagesPerCat);
    results.pakistani = pakRes;
    totalItems += pakRes.items_scraped || 0;
  } catch (err) {
    results.pakistani = { success: false, error: err.message };
  }

  // 3. Bangladeshi
  try {
    const banglaRes = await syncCategory(env, "bangladesi", 1);
    results.bangladesi = banglaRes;
    totalItems += banglaRes.items_scraped || 0;
  } catch (err) {
    results.bangladesi = { success: false, error: err.message };
  }

  return {
    success: true,
    timestamp: new Date().toISOString(),
    total_items_scraped: totalItems,
    categories: results,
  };
}
