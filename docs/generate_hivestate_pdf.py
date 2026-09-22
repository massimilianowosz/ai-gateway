#!/usr/bin/env python3
"""Generate HiveState multi-domain benchmark PDF in academic whitepaper style (ReportLab).

Structure: Results First, then architecture deep-dive.
Includes: "How HiveState Works" example page, Production Considerations,
Ubiquum Optimization Stack final page.
"""
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
from matplotlib.lines import Line2D
import numpy as np
import io, os

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

sCode = S('sCode',
    fontName='Courier', fontSize=8, leading=11,
    textColor=C_TEXT, backColor=HexColor('#f8f8f8'),
    leftIndent=8, rightIndent=8, spaceBefore=3, spaceAfter=3)

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

# Big flow step style for "How it works" page
sFlowStep = S('sFlowStep',
    fontName='Times-Bold', fontSize=11, leading=15,
    textColor=C_BLACK, alignment=TA_CENTER, spaceAfter=2)

sFlowArrow = S('sFlowArrow',
    fontName='Times-Roman', fontSize=11, leading=14,
    textColor=C_LIGHT, alignment=TA_CENTER, spaceAfter=2, spaceBefore=2)

sFlowNote = S('sFlowNote',
    fontName='Times-Italic', fontSize=8.5, leading=11,
    textColor=C_MUTED, alignment=TA_CENTER, spaceAfter=6)

# Stack page styles
sStackTitle = S('sStackTitle',
    fontName='Times-Bold', fontSize=18, leading=22,
    textColor=C_BLACK, alignment=TA_CENTER, spaceAfter=16)

sStackLayer = S('sStackLayer',
    fontName='Times-Bold', fontSize=13, leading=17,
    textColor=C_BLACK, alignment=TA_CENTER, spaceAfter=2)

sStackDesc = S('sStackDesc',
    fontName='Times-Roman', fontSize=10, leading=14,
    textColor=C_MUTED, alignment=TA_CENTER, spaceAfter=4)

sStackArrow = S('sStackArrow',
    fontName='Times-Roman', fontSize=16, leading=20,
    textColor=C_LIGHT, alignment=TA_CENTER, spaceAfter=4)

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

C_BLUE    = '#1a73e8'
C_DKBLUE  = '#0d47a1'
C_LTBLUE  = '#90caf9'
C_GREEN   = '#2e7d32'
C_TEAL    = '#00897b'
C_ORANGE  = '#ef6c00'
C_RED     = '#c62828'
C_PURPLE  = '#6a1b9a'

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

def fig_cross_domain_reduction():
    plt.rcParams.update(MPLRC)
    domains = ['Customer\nService\n(MultiWOZ)', 'Tool Agents\n(Tau-Bench)', 'Coding Agents\n(SWE-smith)', 'Web Agents\n(WebShop)']
    reductions = [54.1, 46.6, 71.5, 60.1]
    colors = [C_BLUE, C_TEAL, C_GREEN, C_ORANGE]
    fig, ax = plt.subplots(figsize=(5.4, 3.0))
    x = np.arange(len(domains))
    bars = ax.bar(x, reductions, color=colors, alpha=0.85, width=0.6, zorder=2)
    mean_red = np.mean(reductions)
    ax.axhline(mean_red, color=C_RED, ls='--', lw=1.2, label=f'Cross-domain mean ({mean_red:.1f}%)')
    ax.set_xticks(x); ax.set_xticklabels(domains, fontsize=8)
    ax.set_ylabel('Token Reduction (%)', fontsize=8); ax.set_ylim(0, 100)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    for bar, v in zip(bars, reductions):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 1.5,
                f'{v}%', ha='center', va='bottom', fontsize=9, fontweight='bold')
    fig.tight_layout(); return fig

def fig_activation_by_domain():
    plt.rcParams.update(MPLRC)
    domains = ['MultiWOZ\n(Chat)', 'Tau-Bench\n(Tool)', 'SWE-smith\n(Code)', 'WebShop\n(Web)']
    activation = [85.0, 87.0, 93.0, 98.0]
    tokens_saved = [40966, 175649, 490322, 66490]
    fig, ax1 = plt.subplots(figsize=(5.4, 3.0))
    ax2 = ax1.twinx(); x = np.arange(len(domains))
    ax1.bar(x, activation, color=C_BLUE, alpha=0.80, width=0.5, zorder=2)
    ax2.plot(x, [t/1000 for t in tokens_saved], color=C_GREEN, marker='D', ms=7, lw=2.0, ls='--', zorder=3)
    ax1.set_ylabel('Activation Rate (%)', fontsize=8, color=C_DKBLUE)
    ax2.set_ylabel('Tokens Saved (thousands)', fontsize=8, color=C_GREEN)
    ax1.set_xticks(x); ax1.set_xticklabels(domains, fontsize=8)
    ax1.set_ylim(0, 105); ax2.set_ylim(0, 600); ax2.tick_params(colors=C_GREEN)
    ax1.grid(axis='y', zorder=0)
    h1 = mpatches.Patch(color=C_BLUE, alpha=0.80, label='Activation Rate (%)')
    h2 = Line2D([0],[0], color=C_GREEN, ls='--', marker='D', ms=6, label='Tokens Saved (K)')
    ax1.legend(handles=[h1, h2], fontsize=7, loc='lower right', framealpha=0.9)
    fig.tight_layout(); return fig

def fig_wcs_by_domain():
    plt.rcParams.update(MPLRC)
    domains = ['MultiWOZ', 'Tau-Bench', 'SWE-smith', 'WebShop']
    wcs_mean = [4.74, 4.50, 4.80, 4.76]
    colors = [C_BLUE, C_TEAL, C_GREEN, C_ORANGE]
    fig, ax = plt.subplots(figsize=(5.4, 2.8))
    x = np.arange(len(domains))
    bars = ax.bar(x, wcs_mean, color=colors, alpha=0.85, width=0.55, zorder=2)
    ax.axhline(5.0, color='#cccccc', ls='-', lw=0.8, zorder=1)
    ax.axhline(4.70, color=C_RED, ls='--', lw=1.2, label='Overall mean (4.70)')
    ax.set_xticks(x); ax.set_xticklabels(domains, fontsize=8)
    ax.set_ylabel('WCS (1\u20135 scale)', fontsize=8); ax.set_ylim(4.0, 5.05)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    for bar, v in zip(bars, wcs_mean):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.01,
                f'{v}', ha='center', va='bottom', fontsize=9, fontweight='bold')
    fig.tight_layout(); return fig

def fig_redundancy_vs_reduction():
    plt.rcParams.update(MPLRC)
    domains = ['MultiWOZ', 'Tau-Bench', 'WebShop', 'SWE-smith']
    x_pos = [1, 2, 3, 4]; reductions = [54.1, 46.6, 60.1, 71.5]
    colors = [C_BLUE, C_TEAL, C_ORANGE, C_GREEN]; sizes = [120, 100, 90, 80]
    fig, ax = plt.subplots(figsize=(5.4, 2.8))
    for xp, red, col, sz, dom in zip(x_pos, reductions, colors, sizes, domains):
        ax.scatter(xp, red, s=sz, color=col, alpha=0.85, zorder=3)
        ax.annotate(dom, (xp, red), textcoords="offset points", xytext=(8, -3), fontsize=8, color=col)
    z = np.polyfit(x_pos, reductions, 1); p = np.poly1d(z)
    ax.plot([0.5, 4.5], [p(0.5), p(4.5)], ls='--', color='#aaaaaa', lw=1.2, zorder=1)
    ax.set_xlabel('Context Redundancy \u2192', fontsize=8)
    ax.set_ylabel('Token Reduction (%)', fontsize=8)
    ax.set_xlim(0.5, 4.8); ax.set_ylim(30, 90)
    ax.set_xticks([1, 2, 3, 4])
    ax.set_xticklabels(['Conversational\nrepetition', 'Verbose API\nresponses',
                        'Superseded\nobservations', 'Full file\ncontents'], fontsize=7)
    ax.grid(zorder=0); fig.tight_layout(); return fig

def fig_cost_by_deployment():
    plt.rcParams.update(MPLRC)
    deploy_types = ['Customer\nService\n(85% act.)', 'Mixed Agent\nWorkloads\n(95% act.)', 'Dedicated Coding\nAgent Platform\n(97% act.)']
    savings_claude = [982, 2394, 15887]; savings_gpt4o = [818, 1995, 13239]
    fig, ax = plt.subplots(figsize=(5.4, 3.0))
    x = np.arange(len(deploy_types)); w = 0.35
    ax.bar(x - w/2, [s/1000 for s in savings_claude], w, color=C_BLUE, alpha=0.85, label='Claude Sonnet 4 ($3/M)')
    ax.bar(x + w/2, [s/1000 for s in savings_gpt4o],  w, color=C_TEAL, alpha=0.85, label='GPT-4o ($2.50/M)')
    ax.set_xticks(x); ax.set_xticklabels(deploy_types, fontsize=8)
    ax.set_ylabel('Monthly Savings ($K)', fontsize=8)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    for i, (sc, sg) in enumerate(zip(savings_claude, savings_gpt4o)):
        ax.text(i - w/2, sc/1000 + 0.3, f'${sc:,}', ha='center', fontsize=6.5, fontweight='bold')
        ax.text(i + w/2, sg/1000 + 0.3, f'${sg:,}', ha='center', fontsize=6.5, fontweight='bold')
    fig.tight_layout(); return fig

def fig_latency():
    plt.rcParams.update(MPLRC)
    domains = ['MultiWOZ', 'Tau-Bench', 'SWE-smith', 'WebShop']
    p50 = [887, 1215, 1768, 922]; p95 = [1103, 2048, 1860, 1461]
    colors = [C_BLUE, C_TEAL, C_GREEN, C_ORANGE]
    fig, ax = plt.subplots(figsize=(5.4, 2.6))
    x = np.arange(len(domains)); w = 0.35
    ax.bar(x - w/2, [v/1000 for v in p50], w, color=colors, alpha=0.85, label='p50')
    ax.bar(x + w/2, [v/1000 for v in p95], w, color=colors, alpha=0.45, label='p95')
    ax.axhline(2.0, color=C_RED, ls=':', lw=1.2, label='2s threshold')
    ax.set_xticks(x); ax.set_xticklabels(domains, fontsize=8)
    ax.set_ylabel('Latency (seconds)', fontsize=8); ax.set_ylim(0, 2.5)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    fig.tight_layout(); return fig

def fig_ccr_mechanism():
    plt.rcParams.update(MPLRC)
    fig, ax = plt.subplots(figsize=(5.4, 2.4))
    ax.set_xlim(0, 10); ax.set_ylim(0, 5); ax.axis('off')
    boxes = [
        (0.5, 3.5, 2.2, 1.0, 'Extraction\nCompletes', C_BLUE),
        (3.5, 3.5, 2.2, 1.0, 'Store History\n+ Keywords', C_TEAL),
        (6.8, 3.5, 2.5, 1.0, 'CCR Cache\n(64 entries, 30m TTL)', '#666666'),
        (0.5, 1.0, 2.2, 1.0, 'New Request\nArrives', C_ORANGE),
        (3.5, 1.0, 2.2, 1.0, 'Keyword\nMatch (\u22652)', C_PURPLE),
        (6.8, 1.0, 2.5, 1.0, 'Budget-Gated\nInjection (<80%)', C_GREEN),
    ]
    for bx, by, bw, bh, label, color in boxes:
        rect = mpatches.FancyBboxPatch((bx, by), bw, bh, boxstyle="round,pad=0.1",
                                        facecolor=color, alpha=0.15, edgecolor=color, lw=1.5)
        ax.add_patch(rect)
        ax.text(bx + bw/2, by + bh/2, label, ha='center', va='center', fontsize=7,
                color=color, fontweight='bold')
    arrow_props = dict(arrowstyle='->', color='#555555', lw=1.5)
    ax.annotate('', xy=(3.5, 4.0), xytext=(2.7, 4.0), arrowprops=arrow_props)
    ax.annotate('', xy=(6.8, 4.0), xytext=(5.7, 4.0), arrowprops=arrow_props)
    ax.annotate('', xy=(3.5, 1.5), xytext=(2.7, 1.5), arrowprops=arrow_props)
    ax.annotate('', xy=(6.8, 1.5), xytext=(5.7, 1.5), arrowprops=arrow_props)
    ax.annotate('', xy=(8.05, 3.5), xytext=(8.05, 2.0), arrowprops=dict(
        arrowstyle='->', color='#555555', lw=1.2, connectionstyle='arc3,rad=0.3'))
    fig.tight_layout(); return fig

def fig_hiveroute_diagram():
    plt.rcParams.update(MPLRC)
    levels = ['Trivial\n(Low effort)', 'Standard\n(Medium effort)', 'Complex\n(High effort)']
    cost_without = [100, 100, 100]; cost_with = [15, 50, 100]
    colors_with = [C_GREEN, C_TEAL, C_BLUE]
    fig, ax = plt.subplots(figsize=(5.4, 2.6))
    x = np.arange(len(levels)); w = 0.35
    ax.bar(x - w/2, cost_without, w, color='#cccccc', alpha=0.7, label='Without HiveRoute (same model)', edgecolor='#999999')
    bars = ax.bar(x + w/2, cost_with, w, color=colors_with, alpha=0.85, label='With HiveRoute (routed)')
    ax.set_xticks(x); ax.set_xticklabels(levels, fontsize=8)
    ax.set_ylabel('Relative Cost per Request (%)', fontsize=8); ax.set_ylim(0, 120)
    ax.legend(fontsize=7.5, framealpha=0.9); ax.grid(axis='y', zorder=0)
    models = ['Small (e.g. 8B)', 'Mid-tier', 'Frontier']
    for i, (bar, model) in enumerate(zip(bars, models)):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 2,
                model, ha='center', fontsize=7, fontstyle='italic', color=colors_with[i])
    fig.tight_layout(); return fig

def fig_activation_curve():
    plt.rcParams.update(MPLRC)
    buckets = ['<400', '400\u2013800', '800\u20131.5K', '1.5\u20133K', '3\u20136K', '>6K']
    activation = [0, 45, 72, 91, 97, 99]; reduction = [0, 38, 48, 55, 64, 74]
    fig, ax1 = plt.subplots(figsize=(5.4, 2.6))
    ax2 = ax1.twinx(); x = np.arange(len(buckets))
    ax1.bar(x, activation, color=C_BLUE, alpha=0.7, width=0.6)
    ax2.plot(x[1:], reduction[1:], color=C_GREEN, marker='o', ms=6, lw=2.0, ls='-', zorder=3)
    ax1.set_ylabel('Activation Rate (%)', fontsize=8, color=C_DKBLUE)
    ax2.set_ylabel('Mean Reduction (%)', fontsize=8, color=C_GREEN)
    ax1.set_xticks(x); ax1.set_xticklabels(buckets, fontsize=7)
    ax1.set_xlabel('Input Token Count', fontsize=8)
    ax1.set_ylim(0, 105); ax2.set_ylim(0, 100); ax2.tick_params(colors=C_GREEN)
    ax1.grid(axis='y', zorder=0)
    h1 = mpatches.Patch(color=C_BLUE, alpha=0.7, label='Activation Rate (%)')
    h2 = Line2D([0],[0], color=C_GREEN, marker='o', ms=5, label='Mean Reduction (%)')
    ax1.legend(handles=[h1, h2], fontsize=7, loc='center right', framealpha=0.9)
    fig.tight_layout(); return fig

# ── Build PDF ─────────────────────────────────────────────────────────────────
def build(outpath):
    doc = SimpleDocTemplate(outpath, pagesize=A4,
        leftMargin=ML, rightMargin=MR, topMargin=MT, bottomMargin=MB,
        title='HiveState: Adaptive State Compression for Long-Running AI Workflows',
        author='Ubiquum Research')

    story = []

    # ══════════════════════════════════════════════════════════════════════════
    # TITLE + ABSTRACT
    # ══════════════════════════════════════════════════════════════════════════
    story.append(Paragraph('HiveState', sMainTitle))
    story.append(Paragraph(
        'Adaptive State Compression<br/>for Long-Running AI Workflows', sSubtitle))
    story.append(Paragraph('Ubiquum Research \u00b7 June 2026', sByline))
    story.append(thin_rule(thickness=0.8))

    story.append(Paragraph('Abstract', sAbstractLabel))
    story.append(Paragraph(
        'Long-running AI workflows\u2014from multi-turn customer service to autonomous coding '
        'agents\u2014accumulate context that grows linearly with interaction length, driving '
        'quadratic cumulative token costs and degrading attention allocation. We present '
        '<b>HiveState</b>, a threshold-gated middleware system that conditionally compresses '
        'conversation and workflow history into a structured JSON state representation only '
        'when accumulated tokens exceed a configurable threshold. The system employs dynamic '
        'conversation profiling, importance-aware history reordering, adaptive extraction '
        'budgets, and a <b>Compress-Cache-Retrieve (CCR)</b> mechanism for proactive context '
        'injection. Additionally, <b>HiveRoute</b> leverages the extracted state to classify '
        'task difficulty and dynamically route requests to cost-appropriate models. '
        'We evaluate across four workflow categories (N=100 per domain, 400 total): '
        'MultiWOZ 2.2, Tau-Bench, SWE-smith, and AgentInstruct WebShop.',
        sAbstract))
    story.append(Paragraph(
        '<i>Keywords:</i> adaptive state compression \u00b7 long-running AI workflows \u00b7 '
        'tool-augmented agents \u00b7 token efficiency \u00b7 inference cost optimization \u00b7 '
        'LLM gateway middleware \u00b7 intelligent model routing',
        sKeywords))

    # ══════════════════════════════════════════════════════════════════════════
    # RESULTS FIRST
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('Results', sSec))

    results = [
        ['Domain', 'N', 'Activated', 'Rate', 'Mean Red.', 'WCS', 'p50 Lat.', 'Tokens Saved'],
        ['MultiWOZ (Customer Service)', '100', '85', '85%', '54.1%', '4.74', '887 ms', '40,966'],
        ['Tau-Bench (Tool Agents)', '100', '87', '87%', '46.6%', '4.50', '1,215 ms', '175,649'],
        ['SWE-smith (Coding Agents)', '100', '93', '93%', '71.5%', '4.80', '1,768 ms', '490,322'],
        ['WebShop (Web Agents)', '100', '98', '98%', '60.1%', '4.76', '922 ms', '66,490'],
        ['Total', '400', '363', '90.8%', '\u2014', '4.70', '\u2014', '773,427'],
    ]
    story.append(make_table(results, [FW*0.26, FW*0.06, FW*0.09, FW*0.07, FW*0.10, FW*0.07, FW*0.12, FW*0.14],
                   align_cols={0:'LEFT'}))
    story.append(Paragraph(
        '<i>Cross-domain results: 773,427 tokens saved across 400 trajectories. '
        'WCS (Workflow Continuation Score) confirms near-perfect semantic preservation '
        '(4.70/5.0 mean, median 5.0 in every domain).</i>', sCaption))

    fig1 = fig_to_img(fig_cross_domain_reduction(), 13)
    story.append(KeepTogether([fig1, Paragraph(
        '<i>Figure 1: Token reduction by workflow domain. Coding agents achieve highest '
        'reduction (71.5%) due to extreme context redundancy from file reads.</i>',
        sCaption)]))

    # Key findings box
    kf_items = [
        Paragraph('Key Findings', sKFLabel),
        Paragraph('\u2022 <b>47\u201371% token reduction</b> across four workflow domains with zero domain-specific tuning.', sKFBullet),
        Paragraph('\u2022 <b>90.8% activation rate</b> (363/400 trajectories)\u2014threshold gate targets longest workflows.', sKFBullet),
        Paragraph('\u2022 <b>WCS 4.70/5.0</b> (median 5.0)\u2014LLM judge confirms compressed state is sufficient for correct continuation.', sKFBullet),
        Paragraph('\u2022 <b>$982\u2013$15,887/month savings</b> per million requests (frontier models).', sKFBullet),
        Paragraph('\u2022 <b>CCR</b> recovers compressed-away context via budget-gated keyword-based retrieval.', sKFBullet),
        Paragraph('\u2022 <b>HiveRoute</b> routes trivial tasks to smaller models at zero additional inference cost.', sKFBullet),
    ]
    story.append(BoxFlowable(kf_items, FW))
    story.append(Spacer(1, 8))

    # ══════════════════════════════════════════════════════════════════════════
    # 1. INTRODUCTION
    # ══════════════════════════════════════════════════════════════════════════
    story.append(PageBreak())
    story.append(Paragraph('1  Introduction', sSec))
    story.append(Paragraph(
        'The deployment of large language models in production systems has evolved from simple '
        'single-turn question answering to complex, multi-step workflows involving tool use, '
        'code generation, web navigation, and extended human-AI collaboration. These '
        'long-running workflows share a fundamental cost structure: each step requires the '
        'full accumulated context as input, causing cumulative token consumption to grow '
        'quadratically with workflow length.',
        sBody))
    story.append(Paragraph(
        'Prior approaches fall into three categories. <b>Truncation methods</b> discard early '
        'context, sacrificing information critical for task completion [1]. <b>Recursive '
        'summarization</b> maintains a running summary updated every turn, incurring per-turn '
        'overhead [3]. <b>Dialogue state tracking (DST)</b> requires domain-specific ontologies '
        'and supervised training [2], unsuitable for heterogeneous modern workflows.',
        sBody))
    story.append(Paragraph(
        'We propose <b>HiveState</b>, a system that combines the structured efficiency of DST '
        'with the generality of zero-shot extraction. This paper contributes: '
        '(1) a domain-agnostic compression architecture with dynamic profiling, importance '
        'scoring, and adaptive budget control; '
        '(2) <b>Compress-Cache-Retrieve (CCR)</b>\u2014proactive context injection that '
        'mitigates information loss; '
        '(3) <b>HiveRoute</b>\u2014state-informed intelligent model routing; and '
        '(4) a large-scale cross-domain evaluation (N=400) with LLM-judge validation.',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # 2. SYSTEM ARCHITECTURE
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('2  System Architecture', sSec))
    story.append(Paragraph(
        'HiveState interposes between client and downstream LLM as transparent middleware. '
        'The processing pipeline comprises eight stages:',
        sBody))
    story.append(Paragraph(
        'Request \u2192 Token Count \u2192 Threshold Gate \u2192 Profile Detection \u2192 '
        'Conversation Zoning \u2192 Importance Scoring \u2192 State Extraction (budget) '
        '\u2192 CCR Injection \u2192 Forward',
        sCode))

    story.append(Paragraph('2.1  Threshold Gate', sSubSec))
    story.append(Paragraph(
        'The threshold gate (\u03c4) partitions requests into two modes. '
        'Below-threshold requests pass through unchanged (NO_OP) with zero overhead. '
        'Above-threshold requests enter the extraction path. An <b>insufficient headroom</b> '
        'pre-check ensures compression is only attempted when meaningful savings are possible: '
        'if the Recent + Last zones already consume >85% of the original token count, the '
        'system short-circuits to passthrough.',
        sBody))
    story.append(Paragraph(
        '<i>Note: Experiments use a fixed 400-token threshold for cross-domain consistency. '
        'Production deployments typically tune thresholds by workload characteristics '
        '(e.g., higher for agent workflows where context accumulates faster, lower for '
        'latency-sensitive chat).</i>',
        sBody))

    story.append(Paragraph('2.2  Dynamic Conversation Profiling', sSubSec))
    story.append(Paragraph(
        'HiveState classifies each conversation into one of three profiles using heuristic '
        'analysis of the message array, auto-adapting preprocessing without user configuration:',
        sBody))
    profiles = [
        ['Profile', 'Detection Rule', 'StepWindow', 'PreprocessMax'],
        ['tool-agent', '>30% messages are tool responses', '1', '0 (truncated separately)'],
        ['code-agent', '>20% messages contain code patterns', '1', '500'],
        ['chat', 'Default', '1', '800'],
    ]
    story.append(KeepTogether([
        make_table(profiles, [FW*0.18, FW*0.42, FW*0.16, FW*0.24],
                   align_cols={0:'LEFT', 1:'LEFT', 3:'LEFT'}),
        Paragraph('<i>Table 1: Dynamic profiling tunes downstream parameters per conversation type.</i>', sCaption),
    ]))

    story.append(Paragraph('2.3  Conversation Zoning', sSubSec))
    story.append(Paragraph(
        'The message array is partitioned into four zones: '
        '<b>System</b> (passed unchanged), '
        '<b>History</b> (compression candidates), '
        '<b>Recent</b> (last N steps, preserved verbatim), and '
        '<b>Last</b> (final user message, forwarded verbatim). '
        'Step-based splitting counts assistant responses, keeping tool call sequences intact.',
        sBody))

    story.append(Paragraph('2.4  Importance Scoring and History Reordering', sSubSec))
    story.append(Paragraph(
        'Each History message is scored on four dimensions (Recency 0.30, Error Signal 0.25, '
        'Decision Density 0.25, Information Density 0.20). Important messages are '
        '<b>reordered to the end</b>, exploiting LLM recency bias\u2014extraction models '
        'attend more strongly to recent tokens. This improves State Recall by 8\u201312% on '
        'tool-agent conversations.',
        sBody))

    story.append(Paragraph('2.5  Adaptive Extraction Budget', sSubSec))
    story.append(Paragraph(
        'Output is constrained: <b>maxOutputTokens = historyTokens / 3</b> (budget floor: 512 to ensure valid JSON, ceiling: 2048). '
        'Three enforcement levels: (1) <i>Soft</i>\u2014conciseness instructions; '
        '(2) <i>Medium</i>\u2014LLM max_tokens set to budget; (3) <i>Hard</i>\u2014post-extraction '
        'progressive field removal until output fits. Actual output: 183\u2013308 tokens regardless '
        'of input length, well below budget, due to Soft enforcement.',
        sBody))

    story.append(Paragraph('2.6  Structured State Extraction', sSubSec))
    story.append(Paragraph(
        'A <b>configurable extraction model</b> produces a fixed-schema JSON capturing the '
        'minimum sufficient state for continuation: intent, identifiers, values, actions_taken, '
        'progress, status, difficulty, and reasoning_effort. The schema is domain-agnostic\u2014'
        'the same output structure handles chat, tool-agents, coding, and web workflows. '
        'Any model supporting structured output (JSON mode) can serve as the extraction backend.',
        sBody))

    # ── 2.7 CCR ──────────────────────────────────────────────────────────────
    story.append(Paragraph('2.7  Compress-Cache-Retrieve (CCR)', sSubSec))
    story.append(Paragraph(
        'CCR recovers relevant original messages from previous compressions when the current '
        'request references similar entities:',
        sBody))
    story.append(Paragraph(
        '\u2022 <b>Store:</b> After extraction, original History + extracted keywords are '
        'cached (SHA-256 key, 64 entries max, 30-minute TTL).',
        sBullet))
    story.append(Paragraph(
        '\u2022 <b>Retrieve:</b> On subsequent requests, keyword overlap with the current '
        'user message is checked. Requires \u22652 matches to avoid false positives.',
        sBullet))
    story.append(Paragraph(
        '\u2022 <b>Inject (budget-gated):</b> Matched messages are injected only if '
        '<font face="Courier" size="8">currentTokens + ccrTokens &lt; originalTokens \u00d7 80%'
        '</font>. This preserves compression savings.',
        sBullet))

    fig_ccr = fig_to_img(fig_ccr_mechanism(), 13, aspect=0.44)
    story.append(KeepTogether([fig_ccr, Paragraph(
        '<i>Figure 2: CCR lifecycle. Store \u2192 keyword match \u2192 budget-gated injection.</i>',
        sCaption)]))

    # ── 2.8 HiveRoute ────────────────────────────────────────────────────────
    story.append(Paragraph('2.8  HiveRoute: State-Informed Intelligent Routing', sSubSec))
    story.append(Paragraph(
        '<b>HiveRoute</b> is embedded within HiveState\u2019s middleware. During extraction, '
        'the LLM classifies <b>difficulty</b> (trivial/standard/complex) and recommends '
        '<b>reasoning_effort</b> (low/medium/high). HiveRoute overrides the target model '
        'based on a <b>configurable routing table</b> and injects provider-appropriate reasoning '
        'parameters (OpenAI <font face="Courier" size="8">reasoning_effort</font> or Anthropic '
        '<font face="Courier" size="8">thinking.budget_tokens</font>). Routing targets are '
        'fully configurable per-team.',
        sBody))
    story.append(Paragraph(
        'Key insight: since HiveState already extracts structured state, adding difficulty '
        'classification costs <b>zero additional inference</b>\u2014both state and routing '
        'metadata come from the same LLM pass.',
        sBody))
    route_levels = [
        ['Difficulty', 'Example Routed Model', 'Reasoning Effort', 'Thinking Budget'],
        ['complex', 'Frontier (e.g. Claude/GPT-4o)', 'high', '16,384 tokens'],
        ['standard', 'Mid-tier (e.g. GPT-OSS/Gemini)', 'medium', '4,096 tokens'],
        ['trivial', 'Lightweight (e.g. 8B local)', 'low', '1,024 tokens'],
    ]
    story.append(KeepTogether([
        make_table(route_levels, [FW*0.16, FW*0.30, FW*0.22, FW*0.32],
                   align_cols={0:'LEFT', 1:'LEFT', 2:'LEFT', 3:'LEFT'}),
        Paragraph('<i>Table 2: Example HiveRoute mapping. All targets configurable per-team.</i>', sCaption),
    ]))

    fig_route = fig_to_img(fig_hiveroute_diagram(), 13, aspect=0.48)
    story.append(KeepTogether([fig_route, Paragraph(
        '<i>Figure 3: HiveRoute cost impact by difficulty level.</i>',
        sCaption)]))

    # ══════════════════════════════════════════════════════════════════════════
    # 3. EXPERIMENTAL DESIGN
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('3  Experimental Design', sSec))

    ds = [
        ['Domain', 'Dataset', 'N', 'Avg. Tokens', 'Nature'],
        ['Customer Service', 'MultiWOZ 2.2 [2]', '100', '865', 'Human-human dialogues'],
        ['Tool Agents', 'Tau-Bench [4]', '100', '3,914', 'Historical tool-calling traces'],
        ['Coding Agents', 'SWE-smith [7]', '100', '7,088', 'Real coding trajectories'],
        ['Web Agents', 'AgentInstruct [8]', '100', '1,114', 'Web shopping sessions'],
    ]
    story.append(KeepTogether([
        make_table(ds, [FW*0.20, FW*0.22, FW*0.08, FW*0.14, FW*0.36],
                   align_cols={0:'LEFT', 1:'LEFT', 4:'LEFT'}),
        Paragraph('<i>Table 3: Evaluation datasets (400 total trajectories).</i>', sCaption),
    ]))

    story.append(Paragraph(
        '<b>Extraction:</b> Llama 3.3 70B Versatile via Groq (T=0, json_object, max_tokens=2048). '
        '<b>Judge:</b> GPT-OSS 120B (T=0.1). '
        '<b>Threshold:</b> 400 tokens. '
        '<b>Timeout:</b> 20s with 3-attempt retry (1s/2s/4s backoff).',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # 4. DETAILED RESULTS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('4  Detailed Results', sSec))

    story.append(Paragraph('4.1  Workflow Continuation Score (WCS)', sSubSec))
    story.append(Paragraph(
        'An independent 120B model evaluates whether compressed state contains sufficient '
        'information for correct workflow continuation (1\u20135 scale, 50 samples per domain):',
        sBody))
    wcs = [
        ['Domain', 'WCS Mean', 'WCS Median', 'N (judged)'],
        ['MultiWOZ (Customer Service)', '4.74', '5.0', '50'],
        ['Tau-Bench (Tool Agents)', '4.50', '5.0', '50'],
        ['SWE-smith (Coding Agents)', '4.80', '5.0', '50'],
        ['WebShop (Web Agents)', '4.76', '5.0', '50'],
        ['Overall', '4.70', '5.0', '200'],
    ]
    story.append(KeepTogether([
        make_table(wcs, [FW*0.36, FW*0.18, FW*0.22, FW*0.24],
                   align_cols={0:'LEFT'}),
        Paragraph('<i>Table 4: WCS results. Median 5.0 across all domains.</i>', sCaption),
    ]))

    fig3 = fig_to_img(fig_wcs_by_domain(), 13)
    story.append(KeepTogether([fig3, Paragraph(
        '<i>Figure 4: WCS by domain. SWE-smith achieves highest WCS (4.80) despite '
        'highest compression\u2014the 70B model distills code into action summaries.</i>',
        sCaption)]))

    story.append(Paragraph('4.2  Activation Adapts to Workflow Length', sSubSec))
    story.append(Paragraph(
        'The threshold gate naturally segments workflows: short interactions (&lt;400 tokens) '
        'never trigger extraction; longer workflows activate with monotonically increasing '
        'reduction. HiveState delivers maximum value where costs are highest.',
        sBody))

    fig_act = fig_to_img(fig_activation_curve(), 12.5, aspect=0.47)
    story.append(KeepTogether([fig_act, Paragraph(
        '<i>Figure 5: Activation and reduction by token count. Zero overhead on short '
        'conversations.</i>', sCaption)]))

    fig2 = fig_to_img(fig_activation_by_domain(), 13)
    story.append(KeepTogether([fig2, Paragraph(
        '<i>Figure 6: Activation rate and total tokens saved by domain.</i>',
        sCaption)]))

    # ══════════════════════════════════════════════════════════════════════════
    # 5. CROSS-DOMAIN ANALYSIS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('5  Cross-Domain Analysis', sSec))

    story.append(Paragraph('5.1  Compression Scales with Redundancy', sSubSec))
    redund = [
        ['Domain', 'Redundancy Pattern', 'Reduction'],
        ['Coding Agents', 'Full file contents become irrelevant after analysis', '71.5%'],
        ['Web Agents', 'Sequential observations superseded by final selection', '60.1%'],
        ['Customer Service', 'Conversational turns with entity reference-back', '54.1%'],
        ['Tool Agents', 'Verbose API responses with repeated JSON schemas', '46.6%'],
    ]
    story.append(KeepTogether([
        make_table(redund, [FW*0.22, FW*0.56, FW*0.22],
                   align_cols={0:'LEFT', 1:'LEFT'}),
        Paragraph('<i>Table 5: Compression correlates with structural redundancy.</i>', sCaption),
    ]))

    fig4 = fig_to_img(fig_redundancy_vs_reduction(), 12.5, aspect=0.50)
    story.append(KeepTogether([fig4, Paragraph(
        '<i>Figure 7: Reduction scales with the proportion of superseded context.</i>',
        sCaption)]))

    story.append(Paragraph('5.2  Multi-Component Synergy', sSubSec))
    synergy = [
        ['Component', 'Addresses'],
        ['Dynamic Profiling', 'Domain mismatch (one config doesn\u2019t fit all)'],
        ['Importance Scoring', 'Critical info lost in long histories'],
        ['Extraction Budget + Post-Truncation', 'State larger than original (no savings)'],
        ['CCR', 'Information loss when history is compressed away'],
        ['Insufficient Headroom Gate', 'Wasted extraction on short conversations'],
        ['HiveRoute', 'Over-provisioning models for trivial tasks'],
    ]
    story.append(KeepTogether([
        make_table(synergy, [FW*0.38, FW*0.62],
                   align_cols={0:'LEFT', 1:'LEFT'}),
        Paragraph('<i>Table 6: Each component addresses a distinct failure mode with independent failure semantics.</i>', sCaption),
    ]))

    # ══════════════════════════════════════════════════════════════════════════
    # 6. COST ANALYSIS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('6  Cost-Effectiveness', sSec))

    net_delta = [
        ['Domain', 'Extraction Cost', 'Downstream Savings', 'Net Delta', 'Margin'],
        ['MultiWOZ', '$0.000788', '$0.001158', '+$0.000370', '1.5\u00d7'],
        ['Tau-Bench', '$0.002460', '$0.006018', '+$0.003558', '2.4\u00d7'],
        ['SWE-smith', '$0.003345', '$0.016857', '+$0.013512', '5.0\u00d7'],
        ['WebShop', '$0.000827', '$0.002052', '+$0.001225', '2.5\u00d7'],
    ]
    story.append(KeepTogether([
        make_table(net_delta, [FW*0.18, FW*0.22, FW*0.26, FW*0.20, FW*0.14],
                   align_cols={0:'LEFT', 1:'LEFT', 2:'LEFT', 3:'LEFT'}),
        Paragraph('<i>Table 7: Every domain is net-positive vs. Claude Sonnet 4 ($3/M input). '
                  'Extraction at Groq pricing ($0.59/M in, $0.79/M out).</i>', sCaption),
    ]))

    monthly = [
        ['Deployment Profile', 'Act. Rate', 'Savings (Claude)', 'Savings (GPT-4o)'],
        ['Customer Service Platform', '85%', '$982/mo', '$818/mo'],
        ['Mixed Agent Workloads', '95%', '$2,394/mo', '$1,995/mo'],
        ['Dedicated Coding Agent', '97%', '$15,887/mo', '$13,239/mo'],
    ]
    story.append(KeepTogether([
        make_table(monthly, [FW*0.30, FW*0.14, FW*0.28, FW*0.28],
                   align_cols={0:'LEFT'}),
        Paragraph('<i>Table 8: Projected monthly savings at 1M requests/month.</i>', sCaption),
    ]))

    fig5 = fig_to_img(fig_cost_by_deployment(), 13)
    story.append(KeepTogether([fig5, Paragraph(
        '<i>Figure 8: Monthly savings by deployment type.</i>', sCaption)]))

    # ══════════════════════════════════════════════════════════════════════════
    # 7. LATENCY
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('7  Latency', sSec))

    lat = [
        ['Domain', 'p50', 'p95', 'Avg Input'],
        ['MultiWOZ', '887 ms', '1,103 ms', '865 tokens'],
        ['Tau-Bench', '1,215 ms', '2,048 ms', '3,914 tokens'],
        ['SWE-smith', '1,768 ms', '1,860 ms', '7,088 tokens'],
        ['WebShop', '922 ms', '1,461 ms', '1,114 tokens'],
    ]
    story.append(KeepTogether([
        make_table(lat, [FW*0.22, FW*0.20, FW*0.20, FW*0.38],
                   align_cols={0:'LEFT', 3:'LEFT'}),
        Paragraph('<i>Table 9: Extraction latency (Groq). All p95 under 2.1s.</i>', sCaption),
    ]))

    fig6 = fig_to_img(fig_latency(), 12.5, aspect=0.47)
    story.append(KeepTogether([fig6, Paragraph(
        '<i>Figure 9: Latency scales with input length; all within acceptable bounds.</i>',
        sCaption)]))

    story.append(Paragraph(
        'For agent workflows, extraction (0.9\u20131.8s) is invisible alongside multi-second '
        'LLM calls. For user-facing chat, async/speculative extraction applies.',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # 8. ABLATION
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('8  Ablation: Model Size', sSec))
    ablation = [
        ['Configuration', 'MultiWOZ', 'Tau-Bench', 'SWE-smith', 'WebShop'],
        ['Llama 3.1 8B (tight truncation)', '4.22', '4.20', '4.20', '4.60'],
        ['Llama 3.3 70B (tight truncation)', '4.11', '4.60', '3.80', '4.60'],
        ['Llama 3.3 70B (relaxed truncation)', '4.82', '4.84', '4.86', '4.92'],
    ]
    story.append(KeepTogether([
        make_table(ablation, [FW*0.36, FW*0.16, FW*0.16, FW*0.16, FW*0.16],
                   align_cols={0:'LEFT'}),
        Paragraph('<i>Table 10: WCS by model size. 70B with relaxed truncation unlocks +1.06 '
                  'WCS on SWE-smith.</i>', sCaption),
    ]))
    story.append(Paragraph(
        'The 70B model with tight truncation performed <i>worse</i> on SWE-smith (3.80) than '
        'the 8B\u2014it needs more code context to leverage its superior reasoning. Extraction '
        'model capability and input preprocessing must be co-tuned.',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # 9. PRODUCTION CONSIDERATIONS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('9  Production Considerations', sSec))
    story.append(Paragraph(
        'Deploying HiveState in production involves tuning several dimensions to workload '
        'characteristics:',
        sBody))
    for item in [
        '<b>Extraction model choice.</b> '
        'The extraction model is fully configurable. Larger models (70B+) deliver highest '
        'quality but require cloud inference; smaller models (8B) run locally with acceptable '
        'quality on chat workloads. Model choice should match the complexity of your dominant '
        'workflow type.',

        '<b>Threshold tuning.</b> '
        'The 400-token threshold used here provides cross-domain consistency for evaluation. '
        'Production deployments benefit from per-workload tuning: higher thresholds for agent '
        'workflows (where context accumulates in large steps), lower for latency-sensitive chat '
        'where even moderate context growth impacts cost.',

        '<b>CCR effectiveness depends on workload patterns.</b> '
        'CCR activates when the same user/project generates multiple requests referencing similar '
        'entities. High-traffic platforms with session continuity see frequent CCR hits; '
        'stateless batch workloads see minimal activation. Monitor CCR hit rate to validate value.',

        '<b>Routing policies are deployment-specific.</b> '
        'HiveRoute\u2019s difficulty\u2192model mapping depends on which models are available '
        'and their cost/quality trade-offs. The default mapping serves as a starting point; '
        'teams should calibrate based on their model portfolio and quality requirements.',
    ]:
        story.append(Paragraph(f'\u2022 {item}', sBullet))

    # ══════════════════════════════════════════════════════════════════════════
    # 10. CONCLUSION
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('10  Conclusion', sSec))
    story.append(Paragraph(
        'HiveState achieves <b>near-perfect workflow continuation</b> (WCS 4.70/5.0) while '
        'reducing context by <b>47\u201371%</b> across four workflow categories. The central '
        'insight: <b>HiveState preserves executable workflow state, not history.</b> By '
        'extracting intent, identifiers, action outcomes, and pending requirements, it '
        'eliminates redundant context while retaining what drives downstream decisions.',
        sBody))
    story.append(Paragraph(
        'Combined with CCR for episodic retrieval and HiveRoute for intelligent routing, '
        'HiveState delivers $982\u2013$15,887/month savings per million requests\u2014with '
        'every domain net-positive after extraction costs. As AI systems increasingly rely on '
        'long-running workflows, domain-agnostic context compression becomes essential '
        'infrastructure.',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # REPRODUCIBILITY
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('Reproducibility', sSec))
    story.append(Paragraph(
        'cd ubiquum-ai-gateway &amp;&amp; python3 test/hivestate_multidomain_bench.py \\<br/>'
        '&nbsp;&nbsp;--domain all -n 100 --api-key $UBIQUUM_API_KEY \\<br/>'
        '&nbsp;&nbsp;--delay 0.1 --judge gpt-oss:120b --judge-n 50 \\<br/>'
        '&nbsp;&nbsp;--output test/results/multidomain_bench_results.json',
        sCode))
    story.append(Paragraph(
        'Gateway: Ubiquum Gateway v1.x (Go) \u00b7 '
        'Extraction: Llama 3.3 70B via Groq \u00b7 '
        'Judge: GPT-OSS 120B \u00b7 '
        'Seed: 42 \u00b7 '
        'Timeout: 20s',
        sBody))

    # ══════════════════════════════════════════════════════════════════════════
    # REFERENCES
    # ══════════════════════════════════════════════════════════════════════════
    story.append(thin_rule())
    story.append(Paragraph('References', sSec))
    refs = [
        '[1] Liu, N.F., et al. \u201cLost in the Middle: How Language Models Use Long '
        'Contexts.\u201d <i>TACL</i>, 12:157\u2013173, 2024.',
        '[2] Budzianowski, P., et al. \u201cMultiWOZ \u2014 A Large-Scale Multi-Domain '
        'Wizard-of-Oz Dataset.\u201d <i>EMNLP</i>, 2018.',
        '[3] Zhang, Y., et al. \u201cDIALOGPT: Large-Scale Generative Pre-training for '
        'Conversational Response Generation.\u201d <i>ACL System Demos</i>, 2020.',
        '[4] Yao, S., et al. \u201cTau-Bench: A Benchmark for Tool-Agent-User Interaction.\u201d '
        '<i>arXiv:2406.12045</i>, 2024.',
        '[5] Packer, C., et al. \u201cMemGPT: Towards LLMs as Operating Systems.\u201d '
        '<i>arXiv:2310.08560</i>, 2023.',
        '[6] Shinn, N., et al. \u201cReflexion: Language Agents with Verbal Reinforcement '
        'Learning.\u201d <i>NeurIPS</i>, 2023.',
        '[7] Yang, J., et al. \u201cSWE-smith: Scaling Data for Software Engineering '
        'Agents.\u201d <i>arXiv:2504.21798</i>, 2025.',
        '[8] Zeng, A., et al. \u201cAgentTuning: Enabling Generalized Agent Abilities for '
        'LLMs.\u201d <i>arXiv:2310.12823</i>, 2023.',
    ]
    for r in refs:
        story.append(Paragraph(r, sRef))

    # ══════════════════════════════════════════════════════════════════════════
    # APPENDIX A: HOW HIVESTATE WORKS
    # ══════════════════════════════════════════════════════════════════════════
    story.append(PageBreak())
    story.append(Paragraph('Appendix A \u2014 How HiveState Works', sSec))
    story.append(Paragraph(
        'We illustrate HiveState with a concrete example drawn from the Tau-Bench evaluation '
        '(retail agent handling an order exchange). After 20 messages the accumulated context '
        'reaches 7,000 tokens\u2014system prompt, tool calls, API responses, and user messages. '
        'HiveState activates when token count exceeds the configured \u03c4 threshold.',
        sBody))
    story.append(Paragraph(
        'The system detects the conversation profile (<i>tool-agent</i>), zones the 18 '
        'historical messages by importance, preserves the most recent step, and extracts a '
        'structured state of just 220 tokens:',
        sBody))

    story.append(Spacer(1, 6))
    json_lines = [
        '{ "intent": "exchange_items",',
        '  "active_constraints": {',
        '    "identifiers": { "order_id": "ORD-9842", "user": "sophia_h" },',
        '    "values": { "item": "Blue Wool Sweater M", "exchange_for": "L" },',
        '    "actions_taken": [',
        '      {"action": "looked up order", "result": "found, delivered"},',
        '      {"action": "verified exchange policy", "result": "eligible"}',
        '    ],',
        '    "progress": { "done": ["verify_order","check_policy"],',
        '        "current": "process_exchange", "next": ["confirm"] }',
        '  },',
        '  "conversation_status": "in_progress",',
        '  "difficulty": "standard", "reasoning_effort": "medium" }',
    ]
    json_block = '<br/>'.join(json_lines)
    story.append(KeepTogether([
        Paragraph(json_block, sCode),
        Paragraph('<i>Listing A.1: Extracted state (220 tokens). The full 7,000-token history is replaced by '
                  'this structured representation for all subsequent requests.</i>', sCaption),
    ]))

    story.append(Paragraph(
        'On the next turn, the downstream model receives only: system prompt + state JSON + '
        'last user message\u2014a <b>96.9% reduction</b> in context size. The agent completes '
        'the task with a perfect score (WCS: 5/5), demonstrating zero information loss.',
        sBody))

    story.append(Spacer(1, 10))
    comparison = [
        ['', 'Before HiveState', 'After HiveState'],
        ['Context size', '7,000 tokens', '220 tokens'],
        ['Includes', 'Full message history', 'Structured state + last message'],
        ['Reduction', '\u2014', '96.9%'],
        ['Task score (WCS)', '5/5', '5/5'],
    ]
    story.append(KeepTogether([
        make_table(comparison, [FW*0.22, FW*0.39, FW*0.39],
                   align_cols={0:'LEFT', 1:'CENTER', 2:'CENTER'}),
        Paragraph('<i>Table A.1: Context before and after compression for the exchange task.</i>', sCaption),
    ]))

    story.append(Spacer(1, 10))
    story.append(thin_rule())
    story.append(Paragraph(
        'The state preserves <b>what matters</b> for continuation: intent, identifiers, '
        'action outcomes, and next steps. Everything else\u2014verbose API responses, repeated '
        'schema elements, superseded intermediate results\u2014is eliminated.',
        sBody))

    doc.build(story)
    print(f'Written: {outpath}')


if __name__ == '__main__':
    from pathlib import Path
    outpath = Path(__file__).parent / 'hivestate-paper.pdf'
    build(str(outpath))
