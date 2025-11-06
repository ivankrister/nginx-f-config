local ngx_timer_at = ngx.timer.at
local ngx_log = ngx.log
local ERR = ngx.ERR
local DEBUG = ngx.DEBUG
local shared_lock = ngx.shared.prefetch_lock
local HTTP_GET = ngx.HTTP_GET

local function prefetch_worker(premature, uri)
    if premature or not uri then
        return
    end

    local ok, err = shared_lock:add(uri, true, 5)
    if not ok then
        if err ~= "exists" then
            ngx_log(ERR, "prefetch add lock failed for ", uri, ": ", err or "unknown")
        end
        return
    end

    local res, capture_err = ngx.location.capture("/__prefetch" .. uri, { method = HTTP_GET })
    if not res then
        ngx_log(ERR, "prefetch subrequest failed for ", uri, ": ", capture_err or "unknown")
        shared_lock:delete(uri)
        return
    end

    if res.status >= 500 then
        ngx_log(ERR, "prefetch upstream error ", res.status, " for ", uri)
        shared_lock:delete(uri)
    else
        ngx_log(DEBUG, "prefetched ", uri, " with status ", res.status)
    end
end

local function normalize_uri(segment)
    if not segment or segment == "" then
        return nil
    end

    if segment:match("^https?://") then
        -- Cross-origin URLs are not prefetched.
        return nil
    end

    if segment:sub(1, 1) == "/" then
        return segment
    end

    local base = ngx.var.uri:match("^(.*/)[^/]*$") or "/"
    return base .. segment
end

local function schedule_prefetch(body)
    local scheduled = 0
    for line in body:gmatch("[^\r\n]+") do
        if line:find("%.ts") and line:sub(1, 1) ~= "#" then
            local uri = normalize_uri(line)
            if uri then
                local ok, err = ngx_timer_at(0, prefetch_worker, uri)
                if not ok then
                    ngx_log(ERR, "failed to schedule prefetch for ", uri, ": ", err or "unknown")
                else
                    scheduled = scheduled + 1
                    if scheduled >= 5 then
                        return
                    end
                end
            end
        end
    end
    return scheduled
end

-- Buffer chunks until we have the full playlist
local chunk = ngx.arg[1]
local eof = ngx.arg[2]

if chunk and chunk ~= "" then
    ngx.ctx.m3u8_buffer = (ngx.ctx.m3u8_buffer or "") .. chunk
end

if eof then
    if ngx.ctx.m3u8_buffer then
        local count = schedule_prefetch(ngx.ctx.m3u8_buffer)
        if count and count > 0 then
            ngx.header["X-Prefetch"] = tostring(count)
        else
            ngx.header["X-Prefetch"] = "0"
        end
        ngx.ctx.m3u8_buffer = nil
    end
end
