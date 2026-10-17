"""Editable review: proposals are visible and remain separate from adoption."""
from PySide6.QtCore import Qt
from PySide6.QtGui import QColor
from PySide6.QtWidgets import (QDialog, QVBoxLayout, QHBoxLayout, QLabel, QPushButton,
    QTabWidget, QWidget, QTableWidget, QTableWidgetItem, QHeaderView, QLineEdit,
    QCheckBox, QPlainTextEdit, QAbstractItemView, QComboBox)
from musicops.app.search_status import SITE_NAMES
from html import escape
from musicops.providers.catalog_web import HOSTS, detail_url
from urllib.parse import urlsplit


def review_source_url(value):
    if detail_url(value): return True
    if not isinstance(value,str) or len(value)>2000:return False
    try:
        parsed=urlsplit(value)
        return parsed.scheme=='https' and parsed.hostname in HOSTS.values() and parsed.port in (None,443) and not (parsed.username or parsed.password)
    except ValueError:return False


class ReviewDialog(QDialog):
    def __init__(self, owner, job):
        super().__init__(owner); self.owner = owner; self.job = job; self.closed = False
        self.setWindowTitle('核对任务 · ' + job.get('label', 'MusicOps')); self.resize(960, 700)
        self.finished.connect(lambda _: setattr(self, 'closed', True))
        layout = QVBoxLayout(self)
        self.summary = QPlainTextEdit(); self.summary.setReadOnly(True); self.summary.setMaximumHeight(100); layout.addWidget(self.summary)
        self.ai_note = QLabel(); self.ai_note.setTextFormat(Qt.PlainText); self.ai_note.setWordWrap(True); self.ai_note.setTextInteractionFlags(Qt.TextSelectableByMouse); layout.addWidget(self.ai_note)
        self.source_links = QLabel(); self.source_links.setWordWrap(True); self.source_links.setOpenExternalLinks(True); layout.addWidget(self.source_links)
        self.tabs = QTabWidget(); layout.addWidget(self.tabs, 1)
        page = QWidget(); form = QVBoxLayout(page)
        hint = QLabel('可手动修改每首曲目的资料。多位艺人用“ / ”分隔。黄色字段是 AI 建议，保存后才采用。'); hint.setWordWrap(True); form.addWidget(hint)
        self.metadata = QTableWidget(0, 4); self.metadata.setHorizontalHeaderLabels(['歌曲', '艺人', '专辑', '完整时长（秒）']); self.metadata.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch); self.metadata.verticalHeader().setVisible(False); self.metadata.verticalHeader().setDefaultSectionSize(44); self.metadata.setAlternatingRowColors(True); form.addWidget(self.metadata)
        self.catalog = QComboBox(); self.catalog.setMinimumContentsLength(25); self.catalog.setSizeAdjustPolicy(QComboBox.AdjustToMinimumContentsLengthWithIcon)
        form.addWidget(self.catalog); self.catalog_use = QPushButton('用所选目录候选填入当前曲目（仍可修改）'); self.catalog_use.clicked.connect(self.use_catalog); form.addWidget(self.catalog_use)
        self.save_metadata = QPushButton('保存已核对资料并继续'); self.save_metadata.clicked.connect(self.submit_metadata); form.addWidget(self.save_metadata)
        self.tabs.addTab(page, '曲目资料')
        page = QWidget(); form = QVBoxLayout(page)
        form.addWidget(QLabel('选择实际需要的音源；最终仍会核对下载音频、曲目和发行。'))
        self.candidates = QTableWidget(0, 6); self.candidates.setHorizontalHeaderLabels(['专辑 / 资源', '艺人', '站点', '格式 / 编码', '做种', '匹配']); self.candidates.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch); self.candidates.verticalHeader().setVisible(False); self.candidates.verticalHeader().setDefaultSectionSize(44); self.candidates.setAlternatingRowColors(True); self.candidates.setSelectionBehavior(QAbstractItemView.SelectRows); self.candidates.setSelectionMode(QAbstractItemView.SingleSelection); self.candidates.setEditTriggers(QAbstractItemView.NoEditTriggers); form.addWidget(self.candidates)
        self.candidate_use = QPushButton('采用所选音源并继续'); self.candidate_use.clicked.connect(self.submit_candidate); form.addWidget(self.candidate_use); self.tabs.addTab(page, '候选音源')
        page = QWidget(); form = QVBoxLayout(page)
        form.addWidget(QLabel('搜索词可自行调整；只在勾选的已启用站点重新检索。'))
        self.query = QLineEdit(); self.query.setMaxLength(300); form.addWidget(self.query)
        self.site_checks = {}; row = QHBoxLayout()
        for site, name in SITE_NAMES.items():
            check = QCheckBox(name); enabled = bool(owner.config.get('providers', {}).get(site, {}).get('enabled')); check.setEnabled(enabled); check.setChecked(enabled and (job.get('search_sites') is None or site in job['search_sites'])); self.site_checks[site] = check; row.addWidget(check)
        row.addStretch(); form.addLayout(row)
        self.site_detail = QPlainTextEdit(); self.site_detail.setReadOnly(True); form.addWidget(self.site_detail)
        self.research = QPushButton('按以上搜索词与站点重新搜索'); self.research.clicked.connect(self.submit_search); form.addWidget(self.research); self.tabs.addTab(page, '重新搜索')
        row = QHBoxLayout(); self.ai_button = QPushButton('AI 辅助核对'); self.ai_button.clicked.connect(self.assist); row.addWidget(self.ai_button); self.result = QLabel(); self.result.setTextFormat(Qt.PlainText); self.result.setWordWrap(True); row.addWidget(self.result, 1); close = QPushButton('关闭'); close.clicked.connect(self.reject); row.addWidget(close); layout.addLayout(row)
        self.populate(job)

    def populate(self, job):
        self.job = job; proposal = job.get('ai_assistance') or {}
        valid = bool(job.get('review_revision')) and proposal.get('revision') == job.get('review_revision')
        if not valid: proposal={}
        self.summary.setPlainText(job.get('message', '') + '\n' + (job.get('detail') or ''))
        self.ai_note.setText(('AI 建议：' + str(proposal.get('message', '')) + '\n' + str(proposal.get('reason', ''))) if valid else '可自行核对，也可让 AI 查询真实目录、推荐候选或搜索词。建议不会自动生效。')
        rows = job.get('prepared') or []; suggestions = proposal.get('metadata') if valid else None
        self.metadata.setRowCount(len(rows))
        for i, track in enumerate(rows):
            original = [track.get('raw_title', ''), ' / '.join(track.get('artists', [])), track.get('album', ''), str(track.get('duration') or '')]
            suggested = suggestions[i] if suggestions and len(suggestions) == len(rows) else {}
            for j, key in enumerate(('title', 'artist', 'album', 'duration')):
                value = str(suggested.get(key, original[j])); item = QTableWidgetItem(value)
                if value != original[j]: item.setBackground(QColor('#574925')); item.setForeground(QColor('#f4cf78')); item.setToolTip('AI 根据真实目录建议；保存前可修改')
                self.metadata.setItem(i, j, item)
        self.catalog_rows = proposal.get('catalog_choices') or job.get('catalog_choices') or []
        fallback=job.get('ai_fallback') or {}
        web_queries=proposal.get('web_queries') or (fallback.get('web_queries') if fallback.get('web_sources') else []) or []
        self.catalog_rows=list(self.catalog_rows)+[{'source':'AI 联网线索（待核实）','title':row.get('title') or row.get('album',''),'artists':row.get('artists',[]),'album':row.get('album',''),'entity_kind':'recording' if row.get('title') else 'release'} for row in web_queries[:2]]
        if not valid and web_queries:
            self.ai_note.setText('AI 联网找到以下名称线索，尚未核实完整发行。可在下方选择填入、修改后保存。\n'+str(fallback.get('web_reason') or ''))
        links = proposal.get('lookup_links') or job.get('lookup_links') or []
        anchors=['<a href="'+escape(row['url'],quote=True)+'">'+escape(('检索 · ' if row.get('kind')=='search_link' else '资料 · ')+(row.get('name') or row.get('source') or '来源'))+'</a>' for row in links if isinstance(row,dict) and review_source_url(row.get('url'))]
        self.source_links.setText('网站核对：'+' · '.join(anchors) if anchors else '')
        self.catalog.clear()
        for candidate in self.catalog_rows:
            self.catalog.addItem(' · '.join([str(candidate.get('source', '')), str(candidate.get('title', '')), ' / '.join(candidate.get('artists', [])), str(candidate.get('album') or '')]))
        self.catalog_use.setEnabled(bool(self.catalog_rows and rows)); self.catalog.setEnabled(bool(self.catalog_rows))
        choices = job.get('choices') or []; self.candidates.setRowCount(len(choices)); self.candidates.clearSelection()
        for i, candidate in enumerate(choices):
            media = candidate.get('media') or {}; recommended = valid and proposal.get('candidate_id') == candidate.get('candidate_id')
            for j, value in enumerate([candidate.get('title', ''), ' / '.join(candidate.get('artists', [])), SITE_NAMES.get(candidate.get('source'), candidate.get('source', '')), str(media.get('format') or '') + ' / ' + str(media.get('encoding') or ''), str(candidate.get('seeders') or 0), 'AI 推荐' if recommended else '精准匹配' if candidate.get('identity_exact') else '需人工核对']):
                item = QTableWidgetItem(value); item.setToolTip(value); self.candidates.setItem(i, j, item)
            if recommended: self.candidates.selectRow(i)
        first = rows[0] if rows else {}; query = proposal.get('search_query') if valid else None
        self.query.setText(query or job.get('search_query') or (' '.join(first.get('artists', [])) + ' ' + first.get('album', '')).strip())
        outcomes = job.get('outcomes') or job.get('source_outcomes') or {}
        self.site_detail.setPlainText('\n'.join(SITE_NAMES.get(site, site) + '：' + str(outcome.get('message') or outcome.get('status') or '') for site, outcome in outcomes.items()) or '尚未保存站点检索结果；重试将重新读取当前连接设置。')
        waiting = job.get('stage') in {'needs_review', 'failed', 'skipped'}
        self.save_metadata.setEnabled(waiting and bool(rows) and (job.get('review_kind') == 'metadata' or job.get('task_type') in {'identification', 'search'}))
        self.candidate_use.setEnabled(job.get('stage') == 'needs_review' and job.get('review_kind') == 'candidate' and bool(choices))
        self.research.setEnabled(waiting and (job.get('task_type') == 'search' or job.get('review_kind') == 'search' or job.get('task_type') == 'manual_search'))
        self.ai_button.setEnabled(waiting)
        kind = job.get('review_kind'); self.tabs.setCurrentIndex(1 if kind == 'candidate' else 2 if kind in {'search', 'error', 'interrupted'} or job.get('stage') in {'failed', 'skipped'} else 0)

    def use_catalog(self):
        index = self.catalog.currentIndex(); row = self.metadata.currentRow()
        if row < 0 and self.metadata.rowCount() == 1: row = 0
        if index < 0 or row < 0: self.result.setText('请先选择要填写的曲目行'); return
        candidate = self.catalog_rows[index]
        values = {1: ' / '.join(candidate.get('artists', [])), 2: candidate.get('album') or (candidate.get('title') if candidate.get('entity_kind') in {'release', 'master'} else '')}
        if candidate.get('entity_kind') == 'recording':
            values.update({0: candidate.get('title'), 3: str(candidate.get('duration') or '')})
        for column, value in values.items():
            if value: self.metadata.setItem(row, column, QTableWidgetItem(str(value)))
        self.result.setText('已填入可编辑字段，请检查后保存')

    def send(self, payload):
        self.result.setText('正在保存核对结果…')
        self.owner.call('review_workflow', {'job_id': self.job['job_id'], 'expected_revision': self.job.get('review_revision'), **payload}, self.saved, lambda error: self.result.setText(error) if not self.closed else None)

    def saved(self, result):
        self.owner.refresh()
        if not self.closed: self.accept()

    def submit_metadata(self):
        rows = [{key: self.metadata.item(i, j).text() for j, key in enumerate(('title', 'artist', 'album', 'duration'))} for i in range(self.metadata.rowCount())]
        self.send({'choice': 'edit_metadata', 'metadata': rows})

    def submit_candidate(self):
        row = self.candidates.currentRow(); choices = self.job.get('choices') or []
        if row < 0 or row >= len(choices): self.result.setText('请选择需要的音源'); return
        self.send({'choice': choices[row]['candidate_id']})

    def submit_search(self):
        sites = [site for site, check in self.site_checks.items() if check.isEnabled() and check.isChecked()]
        if not self.query.text().strip() or not sites: self.result.setText('请填写搜索词并选择至少一个已启用站点'); return
        self.send({'choice': 'research', 'query': self.query.text().strip(), 'sites': sites})

    def assist(self):
        self.ai_button.setEnabled(False); self.result.setText('正在查询真实目录并辅助核对…')
        def done(result):
            if self.closed: return
            if result.get('error'): self.result.setText('AI 辅助未完成：' + str(result['error'])); self.ai_button.setEnabled(True); return
            self.owner.call('get_workflow', {'job_id': self.job['job_id']}, received, failed)
        def received(result):
            if not self.closed: self.populate(result['job']); self.result.setText('建议已填入，请检查后选择采用或修改')
        def failed(error):
            if not self.closed: self.result.setText(error); self.ai_button.setEnabled(True)
        self.owner.call('assist_workflows', {'job_ids': [self.job['job_id']]}, done, failed)
