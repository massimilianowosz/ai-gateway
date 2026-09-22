#!/usr/bin/env python3
"""Generate HiveCache academic paper PDF in ReportLab style.

Same visual style as generate_hivestate_report_pdf.py.
Output: docs/hivecache-paper.pdf
"""
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
from matplotlib.lines import Line2D
import numpy as np
import io, os
from pathlib import Path

from reportlab.lib.pagesizes import A4
from reportlab.lib.units import cm
from reportlab.lib.styles import ParagraphStyle
from reportlab.lib.enums import TA_JUSTIFY, TA_CENTER, TA_LEFT
from reportlab.platypus import (
    SimpleDocTemplate, Paragraph, Spacer, Table, TableStyle,
    Image, HRFlowable, KeepTogether, PageBreak
)
from reportlab.platypus.flowables import Flowable
from reportlab.lib.colors import HexColor

# ── Palette ───────────────────────────────────────────────────────────────────
C_BLACK  = HexColor('#000000')
C_TEXT   = HexColor('#222222')
C_MUTED  = HexColor('#555555')
C_LIGHT  = HexColor('#888888')
C_RULE   = HexColor('#999999')

W, H = A4
ML, MR, MT, MB = 2.2*cm, 2.2*cm, 2.4*cm, 2.4*cm
FW = W - ML - MR

# ── Styles ────────────────────────────────────────────────────────────────────
def S(name, **kw):
    return ParagraphStyle(name, **kw)

sMainTitle = S('sMainTitle',
    fontName='Times-Bold', fontSize=22, leading=26,
    textColor=C_BLACK, alignment=TA_CENTER, spaceAfter=4)

sSubtitle = S('sSubtitle',
    fontName='Times-Roman', fontSize=13, leading=17,
    textColor=C_TEXT, alignment=TA_CENTER, spaceAfter=3)

sByline = S('sByline',
    fontName='Times-Italic', fontSize=10, leading=13,
    textColor=C_MUTED, alignment=TA_CENTER, spaceAfter=10)

sAbstractLabel = S('sAbstractLabel',
    fontName='Times-Bold', fontSize=9.5, leading=12,
    textColor=C_BLACK, alignment=TA_CENTER, spaceAfter=3)

sAbstract = S('sAbstract',
    fontName='Times-Roman', fontSize=9, leading=13,
    textColor=C_TEXT, alignment=TA_JUSTIFY)

sKeywords = S('sKeywords',
    fontName='Times-Italic', fontSize=8.5, leading=12,
    textColor=C_MUTED, alignment=TA_CENTER, spaceBefore=5, spaceAfter=4)

sSec = S('sSec',
    fontName='Times-Bold', fontSize=11, leading=14,
    textColor=C_BLACK, spaceBefore=14, spaceAfter=4)

sSubSec = S('sSubSec',
    fontName='Times-Bold', fontSize=9.5, leading=13,
    textColor=C_BLACK, spaceBefore=8, spaceAfter=3)

sBody = S('sBody',
    fontName='Times-Roman', fontSize=9, leading=14,
    textColor=C_TEXT, alignment=TA_JUSTIFY, spaceAfter=6)

sCaption = S('sCaption',
    fontName='Times-Italic', fontSize=8, leading=11,
    textColor=C_MUTED, alignment=TA_CENTER, spaceBefore=4, spaceAfter=10)

sBullet = S('sBullet',
    fontName='Times-Roman', fontSize=9, leading=13.5,
    textColor=C_TEXT, leftIndent=12, firstLineIndent=-8,
    alignment=TA_JUSTIFY, spaceAfter=3)

sRef = S('sRef',
    fontName='Times-Roman', fontSize=8.5, leading=12,
    textColor=C_TEXT, leftIndent=16, firstLineIndent=-16, spaceAfter=3)

sKFLabel = S('sKFLabel',
    fontName='Times-Bold', fontSize=9, leading=12,
    textColor=C_BLACK, spaceAfter=3)

sKFBullet = S('sKFBullet',
    fontName='Times-Roman', fontSize=9, leading=13,
    textColor=C_TEXT, leftIndent=10, firstLineIndent=-6, spaceAfter=2)

sFooter = S('sFooter',
    fontName='Times-Italic', fontSize=7.5, leading=10,
    textColor=C_LIGHT, alignment=TA_CENTER, spaceBefore=4)

sCode = S('sCode',
    fontName='Courier', fontSize=8, leading=11,
    textColor=C_TEXT, backColor=HexColor('#f8f8f8'),
    leftIndent=8, rightIndent=8, spaceBefore=3, spaceAfter=3)

# ── Helpers ───────────────────────────────────────────────────────────────────
def thin_rule(width=None, thickness=0.5, color=C_RULE):
    return HRFlowable(width=width or '100%', thickness=thickness,
                      color=color, spaceAfter=6, spaceBefore=2)

def make_table(data, col_widths, align_cols=None):
    ts = [
        ('FONTNAME',   (0,0), (-1,0), 'Times-Bold'),
        ('FONTNAME',   (0,1), (-1,-1), 'Times-Roman'),
        ('FONTSIZE',   (0,0), (-1,-1), 8.5),
        ('LEADING',    (0,0), (-1,-1), 12),
        ('TOPPADDING', (0,0), (-1,-1), 5),
        ('BOTTOMPADDING',(0,0),(-1,-1), 5),
        ('LEFTPADDING', (0,0),(-1,-1), 6),
        ('RIGHTPADDING',(0,0),(-1,-1), 6),
        ('TEXTCOLOR',  (0,0), (-1,-1), C_TEXT),
        ('LINEABOVE',  (0,0), (-1,0), 1.0, C_BLACK),
        ('LINEBELOW',  (0,0), (-1,0), 0.5, C_BLACK),
        ('LINEBELOW',  (0,-1),(-1,-1), 1.0, C_BLACK),
        ('ALIGN', (0,0), (-1,-1), 'CENTER'),
        ('VALIGN', (0,0), (-1,-1), 'MIDDLE'),
    ]
    if align_cols:
        for col, aln in align_cols.items():
            ts.append(('ALIGN', (col,0), (col,-1), aln))
    t = Table(data, colWidths=col_widths)
    t.setStyle(TableStyle(ts))
    return t

class BoxFlowable(Flowable):
    def __init__(self, content_paragraphs, avail_width, padding=8):
        super().__init__()
        self.content = content_paragraphs
        self.avail_width = avail_width
        self.padding = padding
        self._heights = []
        self._total_h = 0

    def wrap(self, aw, ah):
        inner_w = self.avail_width - 2 * self.padding
        total_h = self.padding
        self._heights = []
        for p in self.content:
            w2, h = p.wrap(inner_w, 9999)
            self._heights.append(h)
            total_h += h + p.style.spaceAfter
        total_h += self.padding
        self._total_h = total_h
        return self.avail_width, total_h

    def draw(self):
        c = self.canv
        c.setStrokeColor(HexColor('#cccccc'))
        c.setFillColor(HexColor('#f5f5f5'))
        c.setLineWidth(0.5)
        c.roundRect(0, 0, self.avail_width, self._total_h, 3, fill=1, stroke=1)
        y = self._total_h - self.padding
        inner_w = self.avail_width - 2 * self.padding
        for p, h in zip(self.content, self._heights):
            y -= h
            p.drawOn(c, self.padding, y)
            y -= p.style.spaceAfter

# ── Matplotlib config ─────────────────────────────────────────────────────────
MPLRC = {
    'font.family': 'serif',
    'font.size': 8,
    'axes.spines.top': False,
    'axes.spines.right': False,
    'axes.edgecolor': '#aaaaaa',
    'axes.labelcolor': '#222222',
    'xtick.color': '#222222',
    'ytick.color': '#222222',
    'grid.color': '#dddddd',
    'grid.linewidth': 0.5,
    'figure.facecolor': 'white',
}

C_BLUE   = '#1a73e8'
C_DKBLUE = '#0d47a1'
C_GREEN  = '#2e7d32'
C_TEAL   = '#00897b'
C_ORANGE = '#ef6c00'
C_RED    = '#c62828'

def fig_to_img(fig, width_cm, aspect=0.55, dpi=180):
    buf = io.BytesIO()
    fig.savefig(buf, format='png', dpi=dpi, bbox_inches='tight',
                facecolor='white', edgecolor='none')
    plt.close(fig)
    buf.seek(0)
    wp = width_cm * cm
    img = Image(buf, width=wp, height=wp * aspect)
    img._buf = buf
    return img

# ── Figures ───────────────────────────────────────────────────────────────────

def fig_learning_curve():
    """Figure 1: Single-turn vs multi-turn hit rate across phases."""
    plt.rcParams.update(MPLRC)
    fig, ax = plt.subplots(figsize=(5.8, 3.2))
    phases = ['P1\n(cold)', 'P2\n(warm)', 'P3 r1', 'P3 r2', 'P3 r3']
    x = np.arange(len(phases))
    single = [42.3, 42.4, 65.7, 74.1, 76.7]
    multi  = [16.2, 57.9, 31.5, 45.1, 51.2]

    ax.plot(x, single, 'o-', color=C_DKBLUE, lw=2.0, ms=7, label='Single-turn (Banking77)')
    ax.plot(x, multi, 's--', color=C_TEAL, lw=2.0, ms=7, label='Multi-turn (MultiWOZ)')
    ax.axhline(82, color='#cccccc', ls=':', lw=1.0, label='Single-turn ceiling (~82%)')

    for xi, (sv, mv) in enumerate(zip(single, multi)):
        ax.text(xi, sv + 2.0, f'{sv}%', ha='center', fontsize=7.5, color=C_DKBLUE, fontweight='bold')
        ax.text(xi, mv - 4.5, f'{mv}%', ha='center', fontsize=7.5, color=C_TEAL, fontweight='bold')

    ax.set_xticks(x); ax.set_xticklabels(phases, fontsize=8)
    ax.set_ylabel('Hit Rate (%)', fontsize=8); ax.set_ylim(0, 95)
    ax.set_title('Single-turn saturates quickly; multi-turn gains from warm-start pattern accumulation',
                 fontsize=8.5, pad=8)
    ax.legend(fontsize=7.5, loc='center left', framealpha=0.9)
    ax.grid(axis='y', zorder=0)
    fig.tight_layout(); return fig

def fig_domain_analysis():
    """Figure 2: Hit rate by domain across phases."""
    plt.rcParams.update(MPLRC)
    domains = ['Closings', 'Train', 'Hotel', 'Restaurant', 'Attraction', 'Taxi']
    p1_cold = [53.6, 11.8, 7.8, 10.2, 7.1, 4.0]
    p2_warm = [87.5, 64.6, 51.3, 48.3, 49.0, 41.3]
    p3_run3 = [80.0, 56.6, 43.5, 40.2, 49.5, 28.3]

    fig, ax = plt.subplots(figsize=(5.8, 3.2))
    x = np.arange(len(domains)); w = 0.25
    ax.bar(x - w, p1_cold, w, color=C_BLUE, alpha=0.5, label='P1 Cold')
    ax.bar(x, p2_warm, w, color=C_DKBLUE, alpha=0.85, label='P2 Warm')
    ax.bar(x + w, p3_run3, w, color=C_TEAL, alpha=0.85, label='P3 Run 3')

    ax.set_xticks(x); ax.set_xticklabels(domains, fontsize=8)
    ax.set_ylabel('Hit Rate (%)', fontsize=8); ax.set_ylim(0, 105)
    ax.set_title('Entity-specific domains (taxi, attraction) remain hardest across all phases',
                 fontsize=8.5, pad=8)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    fig.tight_layout(); return fig

def fig_turn_position():
    """Figure 3: Hit rate by turn position (cold vs warm)."""
    plt.rcParams.update(MPLRC)
    turns = ['T1', 'T2', 'T3', 'T4', 'T5', 'T6', 'T7', 'T8', 'T9+']
    cold = [18.0, 5.0, 10.1, 12.8, 14.9, 11.4, 25.0, 31.1, 39.1]
    warm = [59.0, 50.0, 51.5, 53.3, 55.3, 55.3, 64.2, 72.2, 81.2]

    fig, ax = plt.subplots(figsize=(5.8, 3.0))
    x = np.arange(len(turns))
    ax.plot(x, cold, 'o-', color=C_BLUE, lw=1.8, ms=6, label='Cold (P1)')
    ax.plot(x, warm, 's-', color=C_DKBLUE, lw=2.0, ms=6, label='Warm (P2)')

    ax.set_xticks(x); ax.set_xticklabels(turns, fontsize=8)
    ax.set_ylabel('Hit Rate (%)', fontsize=8); ax.set_ylim(0, 95)
    ax.set_xlabel('Turn Position in Dialogue', fontsize=8)
    ax.set_title('Later turns are more cacheable \u2014 conversational vocabulary converges after T6',
                 fontsize=8.5, pad=8)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    fig.tight_layout(); return fig

def fig_threshold_precision():
    """Figure 4: Threshold vs hit rate and retrieval precision."""
    plt.rcParams.update(MPLRC)
    thresholds = [0.90, 0.92, 0.94, 0.95]
    hit_rates = [72.0, 52.1, 28.4, 20.0]
    precision = [93.1, 94.5, 96.2, 98.2]

    fig, ax1 = plt.subplots(figsize=(5.4, 2.8))
    ax2 = ax1.twinx()
    x = np.arange(len(thresholds))

    ax1.plot(x, hit_rates, 'o-', color=C_DKBLUE, lw=2.0, ms=7, label='Hit Rate (%)')
    ax2.plot(x, precision, 's-', color=C_ORANGE, lw=2.0, ms=7, label='Retrieval Precision (%)')

    for xi, (hr, pr) in enumerate(zip(hit_rates, precision)):
        ax1.text(xi, hr + 2, f'{hr}%', ha='center', fontsize=7.5, color=C_DKBLUE, fontweight='bold')
        ax2.text(xi, pr + 0.3, f'{pr}%', ha='center', fontsize=7.5, color=C_ORANGE, fontweight='bold')

    ax1.set_xticks(x); ax1.set_xticklabels([f'\u2265{t}' for t in thresholds], fontsize=8)
    ax1.set_xlabel('Min Score Threshold', fontsize=8)
    ax1.set_ylabel('Hit Rate (%)', fontsize=8, color=C_DKBLUE); ax1.set_ylim(0, 85)
    ax2.set_ylabel('Retrieval Precision (%)', fontsize=8, color=C_ORANGE); ax2.set_ylim(91, 100)
    ax1.set_title('Stricter thresholds raise precision \u2014 98.2% at \u2265 0.95',
                  fontsize=8.5, pad=8)
    h1 = Line2D([0],[0], color=C_DKBLUE, marker='o', ms=5, label='Hit Rate (%)')
    h2 = Line2D([0],[0], color=C_ORANGE, marker='s', ms=5, label='Retrieval Precision (%)')
    ax1.legend(handles=[h1, h2], fontsize=7, loc='center left', framealpha=0.9)
    ax1.grid(axis='y', zorder=0)
    fig.tight_layout(); return fig

# ── Build PDF ─────────────────────────────────────────────────────────────────
def build(outpath):
    doc = SimpleDocTemplate(outpath, pagesize=A4,
        leftMargin=ML, rightMargin=MR, topMargin=MT, bottomMargin=MB,
        title='HiveCache: Dual-Embedding Semantic Reuse for Multi-Turn LLM Applications',
        author='Ubiquum Research')

    story = []

    # ══════════════════════════════════════════════════════════════════════════
    # TITLE + ABSTRACT
    # ══════════════════════════════════════════════════════════════════════════
    story.append(Paragraph('HiveCache', sMainTitle))
    story.append(Paragraph(
        'Dual-Embedding Semantic Reuse<br/>for Multi-Turn LLM Applications', sSubtitle))
    story.append(Paragraph('by Ubiquum', sByline))
    story.append(thin_rule(thickness=0.8))

    story.append(Paragraph('Abstract', sAbstractLabel))
    story.append(Paragraph(
        'HiveCache is a lightweight dual-scoring semantic cache for multi-turn LLM workloads. '
        'It intercepts requests at the HTTP middleware layer, scores candidates using a weighted '
        'combination of query and context embeddings, and returns cached responses when similarity '
        'exceeds configurable thresholds. We evaluate it across three operational scenarios \u2014 '
        '<i>cold start</i>, <i>warm generalization</i>, and <i>incremental learning</i> \u2014 '
        'on Banking77 (single-turn, 1,000 queries/run) and MultiWOZ 2.2 (multi-turn, 100 '
        'dialogues/run). Single-turn caches learn intent clusters and saturate after one pass '
        '(42% cold, ~77% after three passes). Multi-turn caches learn reusable conversational '
        'patterns: hit rate rises from 16% cold to 58% on entirely unseen dialogues after a '
        'single warm pass. Retrieval precision ranges from 90.5% to 98.2% depending on threshold. '
        'All mismatches occur between semantically adjacent intent pairs.',
        sAbstract))
    story.append(Paragraph(
        '<i>Keywords:</i> semantic cache \u00b7 LLM cost reduction \u00b7 multi-turn dialogue '
        '\u00b7 dual-embedding scoring \u00b7 retrieval precision',
        sKeywords))

    # Key Findings box
    kf_items = [
        Paragraph('Key Findings', sKFLabel),
        Paragraph('\u2022 Single-turn caches learn intent clusters and saturate after one pass.', sKFBullet),
        Paragraph('\u2022 Multi-turn caches benefit from warm-start pattern accumulation \u2014 16% \u2192 58% on unseen dialogues.', sKFBullet),
        Paragraph('\u2022 Retrieval precision exceeds 90% across all configurations; reaches 98.2% at threshold \u2265 0.95.', sKFBullet),
        Paragraph('\u2022 FAQ workloads exceed 75% steady-state hit rate after three passes of representative traffic.', sKFBullet),
    ]
    story.append(BoxFlowable(kf_items, FW))
    story.append(Spacer(1, 8))

    # ══════════════════════════════════════════════════════════════════════════
    # 1. SYSTEM ARCHITECTURE
    # ══════════════════════════════════════════════════════════════════════════
    story.append(Paragraph('1  System Architecture', sSec))
    story.append(Paragraph(
        'HiveCache is positioned as a lightweight dual-scoring semantic cache for multi-turn '
        'LLM workloads. It embeds both the query and conversation context via a '
        '<b>configurable embedding provider</b> and returns cached responses when similarity '
        'exceeds configurable thresholds. The scoring function is:',
        sBody))
    story.append(Paragraph(
        'DualScore = \u03b1 \u00d7 cos(q<sub>a</sub>, q<sub>b</sub>) + '
        '\u03b2 \u00d7 cos(ctx<sub>a</sub>, ctx<sub>b</sub>)',
        sCode))
    story.append(Paragraph(
        'with \u03b1 = 0.85 and \u03b2 = 0.15, selected empirically during early internal '
        'testing and kept fixed across all experiments reported here. The decomposition '
        'addresses the <i>polysemy problem</i> in dialogue: "Book it for 2 people" resolves '
        'to different targets depending on whether prior context concerns a restaurant or a '
        'hotel. Context is capped at the last 2,000 characters \u2014 earlier turns have '
        'diminishing relevance and introducing them degrades embedding quality [4].',
        sBody))

    story.append(Paragraph('1.1  Response Bands', sSubSec))
    bands = [
        ['Band', 'Score', 'Behavior'],
        ['DIRECT', '\u2265 0.95', 'Return cached response verbatim'],
        ['REUSE', '\u2265 0.90', 'Return cached response as-is'],
        ['TWEAK', '\u2265 0.85', 'Adapt via configurable adaptation model'],
        ['MISS', '< 0.85', 'Full LLM call; cache result'],
    ]
    story.append(KeepTogether([
        make_table(bands, [FW*0.15, FW*0.22, FW*0.63],
                   align_cols={0:'LEFT', 2:'LEFT'}),
        Paragraph('<i>Table 1: Tiered response bands.</i>', sCaption),
    ]))

    # ══════════════════════════════════════════════════════════════════════════
    # 2. EXPERIMENTAL DESIGN
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('2  Experimental Design', sSec))
    story.append(Paragraph(
        'Three phases isolate distinct cache properties. <b>P1</b> (seed 42, cache flushed) '
        'measures cold-start baseline. <b>P2</b> (seed 92, cache retained from P1) tests '
        'generalization: the cache is populated from P1, then evaluated on 100 entirely unseen '
        'dialogues drawn with a different random seed, ensuring no dialogue or query overlap '
        'between phases. <b>P3</b> (seed 142, no flush, 3 consecutive runs) evaluates '
        'incremental growth.',
        sBody))
    story.append(Paragraph(
        'All runs use <b>Gemma 4 12B</b> (completion) and <b>phi4-mini 3.8B</b> (TWEAK '
        'adaptation), both served locally via Ollama to eliminate cloud-latency variability '
        'and ensure reproducible measurements. Datasets: <b>Banking77</b> [5] \u2014 77 '
        'fine-grained banking intents (CC-BY-4.0); <b>MultiWOZ 2.2</b> [6] \u2014 5 '
        'task-oriented domains (Apache 2.0).',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # 3. RESULTS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('3  Results', sSec))

    story.append(Paragraph('3.1  Overview', sSubSec))
    results = [
        ['Phase', 'Banking77 Hit Rate', 'MultiWOZ Hit Rate', 'Retrieval Precision (B77)'],
        ['P1 \u2014 Cold Start', '42.3%', '16.2%', '90.5%'],
        ['P2 \u2014 Warm Generaliz.', '42.4%', '57.9%', '90.6%'],
        ['P3 Run 1', '65.7%', '31.5%', '\u2014'],
        ['P3 Run 2', '74.1%', '45.1%', '\u2014'],
        ['P3 Run 3', '76.7%', '51.2%', '93.1%'],
    ]
    story.append(KeepTogether([
        make_table(results, [FW*0.28, FW*0.22, FW*0.22, FW*0.28],
                   align_cols={0:'LEFT'}),
        Paragraph('<i>Table 2: Hit rates and retrieval precision across all phases.</i>', sCaption),
    ]))

    story.append(Paragraph(
        'Figure 1 illustrates a key asymmetry: single-turn caches learn intent clusters and '
        'saturate quickly \u2014 one cold pass already covers Banking77\u2019s 77 clusters, so '
        'P2 adds only +0.1 pp. Multi-turn caches learn reusable conversational patterns; the '
        '714 turns seen in P1 are sufficient for the cache to match 58% of entirely new P2 '
        'dialogues, a +41.7 pp gain over cold start.',
        sBody))

    fig1 = fig_to_img(fig_learning_curve(), 13, aspect=0.53)
    story.append(KeepTogether([fig1, Paragraph(
        '<i>Figure 1: Single-turn hit rate plateaus after the first pass; multi-turn hit rate '
        'nearly quadruples once the cache is warm.</i>',
        sCaption)]))

    story.append(Paragraph('3.2  Domain Analysis (Multi-Turn)', sSubSec))
    story.append(Paragraph(
        'Entity specificity is the primary predictor of cache difficulty (Figure 2). Taxi and '
        'attraction domains reference unique place names and origin/destination pairs that recur '
        'rarely across dialogues, limiting generalization. Closing sequences, by contrast, draw '
        'from a finite, domain-independent vocabulary and reach 87.5% in warm mode.',
        sBody))

    fig2 = fig_to_img(fig_domain_analysis(), 13, aspect=0.53)
    story.append(KeepTogether([fig2, Paragraph(
        '<i>Figure 2: Warm cache (P2) improves all domains; entity-specific domains '
        '(taxi, attraction) remain hardest.</i>',
        sCaption)]))

    story.append(Paragraph('3.3  Turn-Position Effect', sSubSec))
    story.append(Paragraph(
        'Later turns are systematically more cacheable because task-oriented dialogues undergo '
        '<i>conversational convergence</i>: by turn 7+, exchanges concentrate on confirmations, '
        'closings and slot-filling recaps that share a far smaller vocabulary than early-turn '
        'specification exchanges (Figure 3). The warm-start gain is uniform across all positions '
        '(~40 pp), showing HiveCache accumulates useful patterns at every conversational depth.',
        sBody))

    fig3 = fig_to_img(fig_turn_position(), 13, aspect=0.50)
    story.append(KeepTogether([fig3, Paragraph(
        '<i>Figure 3: Hit rate grows steadily with turn depth as dialogue vocabulary converges; '
        'warm cache lifts every position by ~40 pp.</i>',
        sCaption)]))

    # ══════════════════════════════════════════════════════════════════════════
    # 4. OPERATIONAL COST IMPLICATIONS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('4  Operational Cost Implications', sSec))
    story.append(Paragraph(
        'To illustrate potential savings, we project cloud-equivalent monthly costs for 1M '
        'queries/month (80 input + 150 output tokens/request) at steady-state hit rates using '
        'Q2 2025 public API pricing. These are not costs measured in the local Ollama setup '
        '(zero marginal API cost) but illustrative projections. For a blended workload '
        '(70% FAQ + 30% multi-turn), the effective hit rate is '
        '0.7\u00d776.7% + 0.3\u00d751.2% = <b>69.1%</b>.',
        sBody))

    costs = [
        ['Model', 'No Cache ($/mo)', 'Cached ($/mo)', 'Savings \u2014 Single', 'Savings \u2014 Multi'],
        ['GPT-4o', '$1,700', '$396', '$1,304', '$870'],
        ['Claude Sonnet 4', '$2,490', '$580', '$1,910', '$1,274'],
        ['Gemini 2.5 Pro', '$1,225', '$286', '$939', '$627'],
        ['GPT-4o-mini', '$100', '$23', '$77', '$51'],
    ]
    story.append(KeepTogether([
        make_table(costs, [FW*0.20, FW*0.20, FW*0.18, FW*0.21, FW*0.21],
                   align_cols={0:'LEFT'}),
        Paragraph(
            '<i>Table 3: Illustrative monthly cost projections at steady-state hit rates '
            '(76.7% single-turn; 51.2% multi-turn; 1M queries/month).</i>', sCaption),
    ]))

    # ══════════════════════════════════════════════════════════════════════════
    # 5. DISCUSSION
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('5  Discussion', sSec))

    story.append(Paragraph('5.1  What HiveCache Learns', sSubSec))
    story.append(Paragraph(
        'HiveCache does not learn individual dialogues. Instead, it accumulates reusable '
        'semantic and conversational patterns that recur across otherwise unrelated dialogues. '
        'This is why the warm-start effect is so pronounced in multi-turn workloads: '
        'structurally similar exchanges \u2014 a hotel booking negotiation, a train reservation '
        'confirmation \u2014 share embedding neighborhoods even when the specific entities '
        '(city names, dates, prices) differ. Single-turn caches exhibit no such warm-start '
        'gain because each intent cluster is already fully covered after one random pass.',
        sBody))

    story.append(Paragraph('5.2  Retrieval Precision and Threshold Trade-off', sSubSec))
    story.append(Paragraph(
        'Retrieval precision improves monotonically with stricter thresholds (Figure 4). At '
        'threshold \u2265 0.95, precision reaches 98.2% with only 11 mismatches out of 600 hits. '
        'Notably, no mismatch spans semantically distant categories: all errors occur between '
        'adjacent intent pairs (e.g., <i>declined_transfer</i> \u2194 <i>failed_transfer</i>), '
        'which would often warrant identical responses in production.',
        sBody))

    fig4 = fig_to_img(fig_threshold_precision(), 12.5, aspect=0.50)
    story.append(KeepTogether([fig4, Paragraph(
        '<i>Figure 4: Raising the threshold trades hit rate for retrieval precision \u2014 '
        '98.2% precision at \u2265 0.95 with 20% hit rate.</i>',
        sCaption)]))

    story.append(Paragraph('5.3  Limitations', sSubSec))
    for item in [
        '<b>DIRECT band inflation.</b> Finite dataset sampling causes ~38 verbatim collisions '
        'per 1,000 Banking77 queries (birthday problem). Production DIRECT share is expected '
        'at 5\u201310% of hits, not the 15\u201330% observed here.',
        '<b>Single-machine, sequential traffic.</b> Distributed or concurrent scenarios may '
        'exhibit different latency and race-condition behavior.',
        '<b>Unbounded memory backend.</b> No eviction policy applied; production deployments '
        'with capacity limits may show lower steady-state rates.',
        '<b>English only.</b> Embedding quality for other languages may require threshold '
        're-tuning.',
    ]:
        story.append(Paragraph(f'\u2022 {item}', sBullet))

    # ══════════════════════════════════════════════════════════════════════════
    # 6. CONCLUSIONS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('6  Conclusions', sSec))
    for item in [
        '<b>Pre-warm multi-turn caches.</b> Seeding with 500\u20131,000 representative '
        'conversations eliminates the 16% cold-start period and immediately delivers 50%+ '
        'hit rates.',
        '<b>Cache selectively from turn 5+.</b> Conversational convergence concentrates most '
        'caching value in later turns; early-turn caching risks false positives from '
        'insufficient context.',
        '<b>Monitor per-domain hit rates.</b> Entity-specific domains (taxi, attraction) may '
        'never achieve high rates and should be excluded from savings projections.',
        '<b>Tune threshold by precision requirement.</b> \u2265 0.94 \u2192 96.2% precision '
        'at 28.4% hit rate; \u2265 0.95 \u2192 98.2% precision at 20% hit rate.',
        '<b>Expect logarithmic saturation.</b> ~80% of benefit is captured within the first '
        '3\u20134 passes of representative traffic.',
    ]:
        story.append(Paragraph(f'\u2022 {item}', sBullet))

    # ══════════════════════════════════════════════════════════════════════════
    # REFERENCES
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('References', sSec))
    refs = [
        '[1] Zhu, F., et al. \u201cGPTCache: An Open-Source Semantic Cache for LLM '
        'Applications.\u201d <i>arXiv:2311.07629</i>, 2023.',
        '[2] Karpukhin, V., et al. \u201cDense Passage Retrieval for Open-Domain Question '
        'Answering.\u201d <i>EMNLP</i>, 2020.',
        '[3] Reimers, N. & Gurevych, I. \u201cSentence-BERT: Sentence Embeddings using '
        'Siamese BERT-Networks.\u201d <i>EMNLP</i>, 2019.',
        '[4] Liu, N., et al. \u201cLost in the Middle: How Language Models Use Long '
        'Contexts.\u201d <i>TACL</i>, 2024.',
        '[5] Casanueva, I., et al. \u201cEfficient Intent Detection with Dual Sentence '
        'Encoders.\u201d <i>NLP4ConvAI Workshop, ACL</i>, 2020.',
        '[6] Zang, X., et al. \u201cMultiWOZ 2.2: A Dialogue Dataset with Additional '
        'Annotation Corrections.\u201d <i>NLP4ConvAI Workshop, ACL</i>, 2020.',
        '[7] Muennighoff, N., et al. \u201cMTEB: Massive Text Embedding Benchmark.\u201d '
        '<i>EACL</i>, 2023.',
    ]
    for r in refs:
        story.append(Paragraph(r, sRef))

    doc.build(story)
    print(f'Written: {outpath}')


if __name__ == '__main__':
    outpath = Path(__file__).parent / 'hivecache-paper.pdf'
    build(str(outpath))
