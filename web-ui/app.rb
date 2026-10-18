# frozen_string_literal: true

require "sinatra"
require "json"
require "net/http"
require "uri"
require "fileutils"
require "securerandom"

CONFIG_PATH = File.expand_path("../ipxe-server/config.json", __dir__)

def load_local_go_config
  return {} unless File.exist?(CONFIG_PATH)

  JSON.parse(File.read(CONFIG_PATH))
rescue StandardError
  {}
end

initial_cfg = load_local_go_config
default_ruby_port = (ENV["PORT"] || initial_cfg["ruby_web_port"] || 4567).to_i

set :bind, "0.0.0.0"
set :port, default_ruby_port
set :public_folder, File.join(__dir__, "public")
set :views, File.join(__dir__, "views")
enable :sessions
set :session_secret, ENV["SESSION_SECRET"] || SecureRandom.hex(32)

helpers do
  def current_user
    session[:user]
  end

  def logged_in?
    !current_user.nil?
  end

  def can_manage_images?
    logged_in? && (current_user["role"] == "admin" || current_user["can_manage_images"] == true)
  end

  def can_manage_settings?
    logged_in? && (current_user["role"] == "admin" || current_user["can_manage_settings"] == true)
  end

  def is_admin?
    logged_in? && current_user["role"] == "admin"
  end

  def require_login!
    halt 401, JSON.generate({ "error" => "Authentication required. Please sign in." }) unless logged_in?
  end

  def require_image_permission!
    require_login!
    return if can_manage_images?

    halt 403, JSON.generate({ "error" => "Permission denied: your account is not allowed to add or remove images." })
  end

  def require_settings_permission!
    require_login!
    return if can_manage_settings?

    halt 403, JSON.generate({ "error" => "Permission denied: admin/settings privilege required." })
  end

  def go_api_base
    return ENV["GO_API_URL"] if ENV["GO_API_URL"] && !ENV["GO_API_URL"].empty?

    cfg = load_local_go_config
    http_port = cfg["http_port"] || 80
    "http://127.0.0.1:#{http_port}"
  end

  def go_get_json(path)
    uri = URI.parse("#{go_api_base}#{path}")
    http = Net::HTTP.new(uri.host, uri.port)
    http.open_timeout = 3
    http.read_timeout = 5
    res = http.get(uri.request_uri)
    JSON.parse(res.body)
  rescue StandardError => e
    { "error" => "Go backend unreachable at #{go_api_base} (#{e.message})" }
  end

  def go_post_json(path, payload)
    uri = URI.parse("#{go_api_base}#{path}")
    http = Net::HTTP.new(uri.host, uri.port)
    http.open_timeout = 3
    http.read_timeout = 10
    req = Net::HTTP::Post.new(uri.request_uri, { "Content-Type" => "application/json" })
    req.body = JSON.generate(payload)
    res = http.request(req)
    JSON.parse(res.body)
  rescue StandardError => e
    { "error" => e.message }
  end
end

# Main Management Portal Page (View-Only Analytics when logged out, Full Console when logged in)
get "/" do
  @overview = go_get_json("/api/overview")
  @backend_online = !@overview.key?("error")
  erb :index
end

# Live JSON Poll Endpoint (Redacts sensitive config/users when not logged in)
get "/api/overview" do
  content_type :json
  category = params[:category] ? "?category=#{URI.encode_www_form_component(params[:category])}" : ""
  data = go_get_json("/api/overview#{category}")

  unless logged_in?
    # View-Only Public Mode: redact sync secret, users list, and systemd unit paths
    data.delete("users")
    data.delete("systemd_go")
    data.delete("systemd_ruby")
    if data["config"].is_a?(Hash)
      data["config"] = {
        "node_name" => data["config"]["node_name"],
        "server_ip" => data["config"]["server_ip"],
        "tftp_port" => data["config"]["tftp_port"],
        "http_port" => data["config"]["http_port"],
        "ruby_web_port" => data["config"]["ruby_web_port"],
        "default_wim" => data["config"]["default_wim"]
      }
    end
  end

  data["auth"] = {
    "logged_in" => logged_in?,
    "user" => current_user
  }
  JSON.generate(data)
end

# Authentication Endpoints
post "/auth/login" do
  content_type :json
  payload = JSON.parse(request.body.read)
  res = go_post_json("/api/auth/login", payload)
  if res["status"] == "ok" && res["user"]
    session[:user] = res["user"]
  else
    status 401
  end
  JSON.generate(res)
end

post "/auth/logout" do
  content_type :json
  session.clear
  JSON.generate({ "status" => "logged_out" })
end

post "/auth/change-password" do
  require_login!
  content_type :json
  payload = JSON.parse(request.body.read)
  res = go_post_json("/api/auth/change-password", {
    "username" => current_user["username"],
    "new_password" => payload["new_password"]
  })
  if res["status"] == "ok"
    session[:user]["must_change_password"] = false
  end
  JSON.generate(res)
end

# User Management (Admin Only)
post "/users/save" do
  require_settings_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/users", payload))
end

post "/users/delete" do
  require_settings_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/users/delete", payload))
end

# Update Server Setup & Settings
post "/settings" do
  require_settings_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/config", payload))
end

# Upload OS WIMs (images/os/), WinPE Boot Files (images/winpe/), or Linux Files (images/linux/)
post "/images/upload" do
  require_image_permission!
  content_type :json
  category = (params[:category] || "os").downcase
  file_param = params[:file]

  unless file_param && file_param[:tempfile] && file_param[:filename]
    status 400
    return JSON.generate({ "error" => "No file provided" })
  end

  safe_name = File.basename(file_param[:filename].to_s.strip)
  subdir = case category
           when "os" then "os"
           when "linux" then "linux"
           else "winpe"
           end
  local_images_dir = File.expand_path("../ipxe-server/http_files/images/#{subdir}", __dir__)

  begin
    FileUtils.mkdir_p(local_images_dir)
    dest_path = File.join(local_images_dir, safe_name)
    FileUtils.mv(file_param[:tempfile].path, dest_path)
    FileUtils.chmod(0o644, dest_path)

    if category == "windows" && safe_name.downcase.end_with?(".wim")
      cfg = load_local_go_config
      def_wim = cfg["default_wim"] || "boot.wim"
      unless File.exist?(File.join(local_images_dir, def_wim))
        go_post_json("/api/images/default-wim", { "filename" => safe_name })
      end
    end

    JSON.generate({ "status" => "ok", "saved_files" => [safe_name] })
  rescue StandardError => e
    status 500
    JSON.generate({ "error" => e.message })
  end
end

# Delete an Image File
post "/images/delete" do
  require_image_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/images/delete", payload))
end

# Set Active Default WinPE Boot WIM
post "/images/default-wim" do
  require_image_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/images/default-wim", payload))
end

# Toggle Image Sync Flag for Multi-Site Warehouse Distribution
post "/images/sync-flag" do
  require_image_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/images/sync-flag", payload))
end

# BIOS Manufacturer/Model -> Auto OS WIM Rules
post "/rules/create" do
  require_image_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/rules", payload))
end

post "/rules/delete" do
  require_image_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/rules/delete", payload))
end

# Per-Device Next Boot Queue
post "/devices/queue" do
  require_login!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/devices/queue", payload))
end

# Warehouse Sync Nodes Management
post "/nodes/create" do
  require_settings_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/nodes", payload))
end

post "/nodes/delete" do
  require_settings_permission!
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/nodes/delete", payload))
end

post "/nodes/sync-now" do
  require_image_permission!
  content_type :json
  JSON.generate(go_post_json("/api/nodes/sync-now", {}))
end

post "/nodes/pull-db" do
  require_login!
  content_type :json
  JSON.generate(go_post_json("/api/nodes/pull-db", {}))
end

post "/system/update" do
  require_settings_permission!
  content_type :json
  JSON.generate(go_post_json("/api/system/update", {}))
end

# Trigger a Simulated PXE Hardware Check-In & WIM Download for Testing
post "/simulate-boot" do
  require_login!
  content_type :json
  JSON.generate(go_post_json("/api/simulate-boot", {}))
end

