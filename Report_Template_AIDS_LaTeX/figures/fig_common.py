"""Shared drawing primitives for every report figure.

House style, chosen to match the figures in VLDB / SOSP / CIDR papers rather
than the default matplotlib look:

  * pure white background, no frame, no axis ticks anywhere;
  * every box is a pale fill with a saturated border of the same hue, so the
    figure survives greyscale printing (the fills differ in lightness, not
    only in hue);
  * sans-serif labels on a serif-bodied page, which is the usual convention
    and keeps a diagram visually separate from the prose around it;
  * arrows are thin and dark grey, never black, so they read as connectors
    rather than as content.

Every figure is saved at 300 dpi so it stays sharp when LaTeX scales it down
to the text width.
"""

import matplotlib

matplotlib.use("Agg")

import matplotlib.pyplot as plt
from matplotlib.patches import FancyArrowPatch, FancyBboxPatch, Rectangle

# --- palette -------------------------------------------------------------
# (fill, edge). Fills are deliberately spread across lightness so the figure
# is still readable in a black-and-white print of the report.
PALETTE = {
    "blue":   ("#DCE9F7", "#2B6CB0"),
    "green":  ("#DBF0E3", "#2F855A"),
    "amber":  ("#FDF1D6", "#B7791F"),
    "red":    ("#FBE1E1", "#C53030"),
    "purple": ("#E8E1F5", "#6B46C1"),
    "teal":   ("#D9F0F2", "#2C7A7B"),
    "grey":   ("#EDF0F2", "#4A5568"),
    "white":  ("#FFFFFF", "#4A5568"),
}

INK = "#1A202C"      # text
WIRE = "#4A5568"     # arrows and rules
FAINT = "#A0AEC0"    # guides, dividers

plt.rcParams.update(
    {
        "font.family": "DejaVu Sans",
        "font.size": 9,
        "figure.facecolor": "white",
        "axes.facecolor": "white",
        "savefig.facecolor": "white",
        "savefig.bbox": "tight",
        "savefig.pad_inches": 0.04,
    }
)


# Boxes registered by box() so save() can prove the label actually fits.
# Text that spills past its own border is the one defect in a hand-placed
# diagram that no amount of re-reading the source will reveal, so it is
# measured against the rendered glyphs rather than estimated.
_BOXES = []


def canvas(w, h, xlim=(0, 100), ylim=(0, 100)):
    """A blank, white, unitless drawing surface."""
    fig, ax = plt.subplots(figsize=(w, h))
    ax.set_xlim(*xlim)
    ax.set_ylim(*ylim)
    ax.set_aspect("auto")
    ax.axis("off")
    return fig, ax


def box(
    ax,
    x,
    y,
    w,
    h,
    text,
    colour="blue",
    size=9,
    bold=False,
    radius=1.4,
    lw=1.3,
    dashed=False,
    text_colour=None,
    zorder=3,
):
    """A rounded rectangle with a centred label. (x, y) is its centre."""
    fill, edge = PALETTE[colour]
    patch = FancyBboxPatch(
        (x - w / 2, y - h / 2),
        w,
        h,
        boxstyle=f"round,pad=0,rounding_size={radius}",
        facecolor=fill,
        edgecolor=edge,
        linewidth=lw,
        linestyle=(0, (4, 2)) if dashed else "solid",
        zorder=zorder,
        mutation_aspect=1,
    )
    ax.add_patch(patch)
    if text:
        artist = ax.text(
            x,
            y,
            text,
            ha="center",
            va="center",
            fontsize=size,
            color=text_colour or INK,
            fontweight="bold" if bold else "normal",
            linespacing=1.35,
            zorder=zorder + 1,
        )
        _BOXES.append((ax, artist, w, h, text))
    return patch


def band(ax, x, y, w, h, title, colour="grey", size=8.5, lw=1.1, title_dx=1.0):
    """A pale container drawn behind a group of boxes, labelled top-left."""
    fill, edge = PALETTE[colour]
    ax.add_patch(
        Rectangle(
            (x - w / 2, y - h / 2),
            w,
            h,
            facecolor=fill,
            edgecolor=edge,
            linewidth=lw,
            linestyle=(0, (5, 3)),
            zorder=1,
            alpha=0.5,
        )
    )
    if title:
        ax.text(
            x - w / 2 + title_dx,
            y + h / 2 - 1.5,
            title,
            ha="left",
            va="top",
            fontsize=size,
            color=edge,
            fontweight="bold",
            zorder=2,
        )


def arrow(
    ax,
    p,
    q,
    label=None,
    colour=WIRE,
    style="-|>",
    lw=1.15,
    rad=0.0,
    size=8,
    label_pos=0.5,
    dx=0,
    dy=1.6,
    dashed=False,
    label_colour=None,
    zorder=4,
):
    """Arrow from p=(x,y) to q=(x,y), optionally labelled near its midpoint."""
    ax.add_patch(
        FancyArrowPatch(
            p,
            q,
            arrowstyle=style,
            mutation_scale=10,
            linewidth=lw,
            color=colour,
            linestyle=(0, (4, 2)) if dashed else "solid",
            connectionstyle=f"arc3,rad={rad}",
            shrinkA=1.5,
            shrinkB=1.5,
            zorder=zorder,
        )
    )
    if label:
        mx = p[0] + (q[0] - p[0]) * label_pos + dx
        my = p[1] + (q[1] - p[1]) * label_pos + dy
        ax.text(
            mx,
            my,
            label,
            ha="center",
            va="center",
            fontsize=size,
            color=label_colour or colour,
            zorder=zorder + 1,
            bbox=dict(boxstyle="round,pad=0.18", fc="white", ec="none", alpha=0.92),
        )


def label(
    ax,
    x,
    y,
    text,
    size=9,
    bold=False,
    colour=None,
    ha="center",
    va="center",
    rot=0,
    mono=False,
    zorder=6,
    bg=False,
):
    kw = {}
    if bg:
        kw["bbox"] = dict(boxstyle="round,pad=0.2", fc="white", ec="none", alpha=0.92)
    ax.text(
        x,
        y,
        text,
        ha=ha,
        va=va,
        fontsize=size,
        color=colour or INK,
        fontweight="bold" if bold else "normal",
        rotation=rot,
        linespacing=1.35,
        family="DejaVu Sans Mono" if mono else "DejaVu Sans",
        zorder=zorder,
        **kw,
    )


#: A label is never shrunk below this. Past it the text stops being readable
#: once LaTeX scales the figure down to the text width, so a box that still
#: overflows at this size is a layout mistake to fix by hand, not by shrinking.
MIN_PT = 6.2


def _fit_labels(fig, name):
    """Shrink any label that overruns its own box until it fits.

    Measurement is done on the rendered glyphs and converted back into the
    axes' own units, so it uses real font metrics rather than an assumed
    average character width. Anything still too large at MIN_PT is reported
    instead of silently clipped.
    """
    fig.canvas.draw()
    renderer = fig.canvas.get_renderer()
    bad = []
    for ax, artist, w, h, text in _BOXES:
        if artist.axes is not ax or ax.figure is not fig:
            continue
        # 0.94 leaves a sliver of padding; text any closer reads as touching
        # the border even when it technically fits inside it.
        for _ in range(24):
            bb = artist.get_window_extent(renderer).transformed(
                ax.transData.inverted()
            )
            if bb.width <= w * 0.94 and bb.height <= h * 0.94:
                break
            pt = artist.get_fontsize()
            if pt <= MIN_PT:
                bad.append(
                    "    %5.1f x%-5.1f box needs %5.1f x%-5.1f at %.1fpt : %s"
                    % (w, h, bb.width, bb.height, pt,
                       text.replace("\n", " / ")[:54])
                )
                break
            artist.set_fontsize(max(MIN_PT, pt * 0.94))
        else:
            continue
    if bad:
        print("  DOES NOT FIT in", name)
        print("\n".join(bad))
    _BOXES.clear()


def save(fig, name, outdir="."):
    import os

    _fit_labels(fig, name)
    path = os.path.join(outdir, name)
    fig.savefig(path, dpi=300)
    plt.close(fig)
    print("wrote", path)
