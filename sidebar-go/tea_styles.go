package main

import "github.com/charmbracelet/lipgloss"

// Lipgloss styles. Rebuilt from config on startup and config reload via
// rebuildStyles(). All color values come from Cfg().Theme so users can
// swap palettes via config.toml [theme] section.

var (
	colorPeach          lipgloss.Color
	colorGreen          lipgloss.Color
	colorYellow         lipgloss.Color
	colorBlue           lipgloss.Color
	colorMauve          lipgloss.Color
	colorDim            lipgloss.Color
	colorLavender       lipgloss.Color
	colorYellowStale    lipgloss.Color
	colorYellowVeryStale lipgloss.Color

	styleBorderDim             lipgloss.Style
	styleBorderActive          lipgloss.Style
	styleActiveTitle           lipgloss.Style
	styleActiveTitleBanner     lipgloss.Style
	styleWindowActiveTitle     lipgloss.Style
	styleBorderPeach           lipgloss.Style
	styleBorderGreen           lipgloss.Style
	styleBorderYellow          lipgloss.Style
	styleBorderYellowStale     lipgloss.Style
	styleBorderYellowVeryStale lipgloss.Style
	styleBorderCursor          lipgloss.Style
	stylePeachBold             lipgloss.Style
	styleEnterMark             lipgloss.Style
	styleBorderMarch           lipgloss.Style
	styleDoneMarch             lipgloss.Style
	styleWorkingMarch          lipgloss.Style

	styleSession   lipgloss.Style
	styleIntent    lipgloss.Style
	styleIntentDim lipgloss.Style
	styleLocation  lipgloss.Style
	styleGit       lipgloss.Style
	stylePreview   lipgloss.Style
	styleFooter    lipgloss.Style

	styleCursorBody    lipgloss.Style
	styleCursorPreview lipgloss.Style

	rainbowColors          []lipgloss.Color
	rainbowStylePtrs       []*lipgloss.Style
	rainbowMarchStylePtrs  []*lipgloss.Style
)

func init() {
	rebuildStylesFrom(defaultConfig().Theme)
}

// rebuildStyles rebuilds all style vars from the current config.
func rebuildStyles() {
	rebuildStylesFrom(Cfg().Theme)
}

// rebuildStylesFrom rebuilds all style vars from the given theme.
// Separated from rebuildStyles to avoid lock re-entry when called
// from ReloadConfig (which already holds globalCfgMu).
func rebuildStylesFrom(t ThemeConfig) {

	colorPeach = lipgloss.Color(t.Peach)
	colorGreen = lipgloss.Color(t.Green)
	colorYellow = lipgloss.Color(t.Yellow)
	colorBlue = lipgloss.Color(t.Blue)
	colorMauve = lipgloss.Color(t.Mauve)
	colorDim = lipgloss.Color(t.Dim)
	colorLavender = lipgloss.Color(t.Lavender)
	colorYellowStale = lipgloss.Color(t.YellowStale)
	colorYellowVeryStale = lipgloss.Color(t.YellowVeryStale)

	styleBorderDim = lipgloss.NewStyle().Foreground(colorDim)
	styleBorderActive = lipgloss.NewStyle().Foreground(colorBlue).Bold(true)
	styleActiveTitle = lipgloss.NewStyle().Foreground(colorBlue).Bold(true)
	styleActiveTitleBanner = lipgloss.NewStyle().Foreground(colorLavender).Bold(true)
	styleWindowActiveTitle = lipgloss.NewStyle().Foreground(colorBlue)
	styleBorderPeach = lipgloss.NewStyle().Foreground(colorPeach)
	styleBorderGreen = lipgloss.NewStyle().Foreground(colorGreen)
	styleBorderYellow = lipgloss.NewStyle().Foreground(colorYellow)
	styleBorderYellowStale = lipgloss.NewStyle().Foreground(colorYellowStale)
	styleBorderYellowVeryStale = lipgloss.NewStyle().Foreground(colorYellowVeryStale)
	styleBorderCursor = lipgloss.NewStyle().Foreground(colorMauve).Bold(true)
	stylePeachBold = lipgloss.NewStyle().Foreground(colorPeach).Bold(true)
	styleEnterMark = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)
	styleBorderMarch = lipgloss.NewStyle().Foreground(colorLavender).Bold(true)
	styleDoneMarch = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)
	styleWorkingMarch = lipgloss.NewStyle().Foreground(colorGreen).Bold(true)

	styleSession = lipgloss.NewStyle().Bold(true)
	styleIntent = lipgloss.NewStyle()
	styleIntentDim = lipgloss.NewStyle().Faint(true)
	styleLocation = lipgloss.NewStyle().Faint(true)
	styleGit = lipgloss.NewStyle().Faint(true)
	stylePreview = lipgloss.NewStyle().Faint(true).Italic(true)
	styleFooter = lipgloss.NewStyle().Faint(true)

	styleCursorBody = lipgloss.NewStyle().Foreground(colorMauve)
	styleCursorPreview = lipgloss.NewStyle().Foreground(colorMauve).Italic(true)

	rainbowColors = make([]lipgloss.Color, len(t.Rainbow))
	for i, hex := range t.Rainbow {
		rainbowColors[i] = lipgloss.Color(hex)
	}

	rainbowStylePtrs = make([]*lipgloss.Style, len(rainbowColors))
	for i, c := range rainbowColors {
		s := lipgloss.NewStyle().Foreground(c)
		rainbowStylePtrs[i] = &s
	}

	rainbowMarchStylePtrs = make([]*lipgloss.Style, len(rainbowColors))
	for i, c := range rainbowColors {
		s := lipgloss.NewStyle().Foreground(c).Bold(true)
		rainbowMarchStylePtrs[i] = &s
	}
}

func rainbowStylePtr(n int) *lipgloss.Style {
	if n < 0 {
		n = -n
	}
	return rainbowStylePtrs[n%len(rainbowStylePtrs)]
}

func rainbowMarchStylePtr(n int) *lipgloss.Style {
	if n < 0 {
		n = -n
	}
	return rainbowMarchStylePtrs[n%len(rainbowMarchStylePtrs)]
}
