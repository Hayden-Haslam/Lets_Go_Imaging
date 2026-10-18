# frozen_string_literal: true

require "sinatra"
require "json"
require "net/http"
require "uri"
require "fileutils"

# Read shared config from ../ipxe-server/config.json if available to auto-configure ports
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

helpers do
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

# Main Management Portal Page
get "/" do
  @overview = go_get_json("/api/overview")
  @backend_online = !@overview.key?("error")
  erb :index
end

# Live JSON Poll Endpoint for Real-time Transfers, Devices, Images, and Logs
get "/api/overview" do
  content_type :json
  category = params[:category] ? "?category=#{URI.encode_www_form_component(params[:category])}" : ""
  JSON.generate(go_get_json("/api/overview#{category}"))
end

# Update Server Setup & Settings
post "/settings" do
  content_type :json
  payload = JSON.parse(request.body.read)
  result = go_post_json("/api/config", payload)
  JSON.generate(result)
end

# Upload Windows (.wim) or Linux (vmlinuz / initrd.img / ISO) Image Files
post "/images/upload" do
  content_type :json
  category = (params[:category] || "windows").downcase
  file_param = params[:file]

  unless file_param && file_param[:tempfile] && file_param[:filename]
    status 400
    return JSON.generate({ "error" => "No file provided" })
  end

  safe_name = File.basename(file_param[:filename].to_s.strip)
  subdir = category == "linux" ? "linux" : "winpe"
  local_images_dir = File.expand_path("../ipxe-server/http_files/images/#{subdir}", __dir__)

  begin
    FileUtils.mkdir_p(local_images_dir)
    dest_path = File.join(local_images_dir, safe_name)
    FileUtils.mv(file_param[:tempfile].path, dest_path)
    FileUtils.chmod(0o644, dest_path)

    # If a .wim file was uploaded and no default wim exists yet, set it on the Go server
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
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/images/delete", payload))
end

# Set Active Default Windows WIM Image
post "/images/default-wim" do
  content_type :json
  payload = JSON.parse(request.body.read)
  JSON.generate(go_post_json("/api/images/default-wim", payload))
end

# Trigger a Simulated PXE Hardware Check-In & WIM Download for Testing
post "/simulate-boot" do
  content_type :json
  JSON.generate(go_post_json("/api/simulate-boot", {}))
end
