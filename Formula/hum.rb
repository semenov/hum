class Hum < Formula
  desc "Fast, zero-config local LLM server for Apple Silicon"
  homepage "https://github.com/semenov/hum"
  head "https://github.com/semenov/hum.git", branch: "main"
  license "MIT"

  depends_on "go" => :build
  depends_on "python@3.12"
  depends_on arch: :arm64
  depends_on :macos

  def install
    venv_python = libexec/"venv/bin/python"

    # The worker needs mlx-lm, which only exists for Apple Silicon, plus
    # llguidance for the grammar that constrains tool calls. They live in their
    # own virtualenv so nothing is installed into the user's Python.
    system Formula["python@3.12"].opt_bin/"python3.12", "-m", "venv", libexec/"venv"
    system venv_python, "-m", "pip", "install", "--quiet", "--upgrade", "pip"
    system venv_python, "-m", "pip", "install", "--quiet", "mlx-lm", "llguidance"

    libexec.install "worker.py"

    # Compile the paths in, so the binary works from anywhere without config.
    ldflags = %W[
      -s -w
      -X main.builtinPython=#{venv_python}
      -X main.builtinWorker=#{libexec}/worker.py
      -X main.version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags.join(" "))
  end

  def caveats
    <<~EOS
      The first `hum start` downloads a model chosen for this Mac's memory —
      up to 20 GB, once. It goes to ~/.hum/models.

        hum start     serve on http://127.0.0.1:4242/v1
        hum chat      talk to it in the terminal
    EOS
  end

  test do
    assert_match "hum", shell_output("#{bin}/hum version")
    # No server is running here, so this must say so rather than hang.
    assert_match "not running", shell_output("#{bin}/hum status")
  end
end
