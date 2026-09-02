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

    # requirements.txt is a lockfile: every package, including the transitive
    # ones, pinned to the version hum was measured against and checked by
    # sha256. --require-hashes makes pip refuse to resolve anything, so two
    # machines installing a month apart get the same worker.
    #
    # This matters more than it looks. The worker reads mlx-lm's BatchGenerator
    # and transformers' tokenizer internals -- `_tokenizer`, `_byte_decoder` --
    # and hands the tokenizer to llguidance. Pinning mlx-lm alone would leave
    # `mlx>=0.31.2` and `transformers>=5.0.0` free to move underneath it.
    system venv_python, "-m", "pip", "install", "--quiet", "--require-hashes",
           "-r", "requirements.txt"

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
      The first `hum start` downloads the model — 20 GB, once. It goes to
      ~/.hum/models. hum needs an Apple Silicon Mac with 32 GB or more.

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
