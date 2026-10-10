require "json"

module OS
  def self.mac?
    ARGV[2] == "darwin"
  end
end

class LegacyCask
  attr_reader :calls, :download_url, :digest

  def initialize
    @calls = []
  end

  # Homebrew 5 reports unknown DSL methods as supported.
  def respond_to_missing?(*); true; end
  def method_missing(*); end
  def staged_path; "/fixture/stage"; end
  def version(value = nil)
    @version = value unless value.nil?
    @version
  end
  def on_macos(&block); instance_eval(&block) if OS.mac?; end
  def on_linux(&block); instance_eval(&block) unless OS.mac?; end
  def on_intel(&block); instance_eval(&block) if ARGV[3] == "amd64"; end
  def on_arm(&block); instance_eval(&block) if ARGV[3] == "arm64"; end
  def postflight(&block); instance_eval(&block); end

  def system_command(command, args:)
    @calls << [command, *args]
  end

  def url(value, **options)
    raise "deprecated verified URL option" if options.key?(:verified)
    @download_url = value
  end

  def sha256(value)
    @digest = value
  end
end

class StepsCask < LegacyCask
  def postflight(*)
    raise "modern Homebrew used legacy postflight"
  end

  def postflight_steps(&block)
    instance_eval(&block)
  end

  def run(command, args:)
    @calls << [command, *args.map { |arg| arg.gsub("{{staged_path}}", staged_path) }]
  end
end

def cask(_name, &block)
  dsl = (ARGV[1] == "steps" ? StepsCask : LegacyCask).new
  dsl.instance_eval(&block)
  puts JSON.generate(calls: dsl.calls, url: dsl.download_url, sha: dsl.digest)
end

load ARGV[0]
