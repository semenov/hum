# Recorded by assets/make-demo.sh, which also drops the idle frames afterwards.
# The commands here are exactly the ones the README tells people to run.
Output assets/demo-raw.gif

Set Shell zsh
Set FontSize 16
Set Width 1160
Set Height 660
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms

Sleep 600ms
Type "brew tap semenov/hum https://github.com/semenov/hum" Sleep 300ms Enter
Sleep 7s
Type "brew trust semenov/hum" Sleep 300ms Enter
Sleep 3s
Type "brew install --HEAD hum" Sleep 300ms Enter
Sleep 30s

Type "clear" Enter
Sleep 300ms

Type "hum start" Sleep 300ms Enter
Sleep 14s

Type "hum ask 'name three roman emperors, one line each'" Sleep 300ms Enter
Sleep 14s

Sleep 1500ms
