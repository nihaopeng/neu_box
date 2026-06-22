"""统一数据库模块 — SQLite 持久化，供 Master 侧各模块使用。

特性:
  - WAL 模式，线程本地连接，自动建表
  - 当前表: experiments（实验笔记）

块（block）模型:
  实验由多个 block 组成，按顺序排列:
    {"type": "text",  "content": "markdown 文本"}
    {"type": "task",  "task_id": "...", "node_id": "...",
     "command": "...", "log": { "status": "...", "result": {...} }}

用法:
    from src_manager.db import Database
    db = Database.get_instance()
"""

import json
import os
import sqlite3
import threading
import time
import uuid


class Database:
    """SQLite 数据库单例（线程安全）。"""

    _instance = None

    def __init__(self, db_path: str = None):
        if db_path is None:
            db_dir = os.getenv('db_dir', os.path.join(os.path.dirname(__file__), '..', 'db'))
            db_path = os.path.join(db_dir, 'master.db')
        self._db_path = os.path.abspath(db_path)
        os.makedirs(os.path.dirname(self._db_path), exist_ok=True)
        self._local = threading.local()
        self._init_tables()

    @classmethod
    def get_instance(cls) -> 'Database':
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    def _get_conn(self) -> sqlite3.Connection:
        if not hasattr(self._local, 'conn') or self._local.conn is None:
            conn = sqlite3.connect(self._db_path, check_same_thread=False)
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA foreign_keys=ON")
            conn.row_factory = sqlite3.Row
            self._local.conn = conn
        return self._local.conn

    def _init_tables(self):
        conn = self._get_conn()
        conn.executescript('''
            CREATE TABLE IF NOT EXISTS experiments (
                id          TEXT PRIMARY KEY,
                title       TEXT    NOT NULL,
                blocks      TEXT    DEFAULT '[]',
                tags        TEXT    DEFAULT '[]',
                created_by  TEXT    DEFAULT '',
                folder_id   TEXT    DEFAULT NULL,
                created_at  REAL,
                updated_at  REAL,
                FOREIGN KEY (folder_id) REFERENCES folders(id) ON DELETE SET NULL
            );
            CREATE INDEX IF NOT EXISTS idx_exp_created    ON experiments(created_at);
            CREATE INDEX IF NOT EXISTS idx_exp_created_by ON experiments(created_by);

            CREATE TABLE IF NOT EXISTS folders (
                id          TEXT PRIMARY KEY,
                name        TEXT    NOT NULL,
                parent_id   TEXT    DEFAULT NULL,
                created_at  REAL,
                FOREIGN KEY (parent_id) REFERENCES folders(id) ON DELETE CASCADE
            );
            CREATE INDEX IF NOT EXISTS idx_folder_parent ON folders(parent_id);
        ''')

        # 迁移旧字段 → blocks（如有旧数据）
        try:
            conn.execute("SELECT description FROM experiments LIMIT 1")
            rows = conn.execute(
                "SELECT id, title, description, results, "
                "node_id, task_id, command, task_log, "
                "created_by, created_at, updated_at "
                "FROM experiments WHERE description != '' OR results != '' OR task_id != ''"
            ).fetchall()
            for r in rows:
                blocks = []
                if r['description']:
                    blocks.append({'type': 'text', 'content': r['description']})
                if r['task_id']:
                    task_log = None
                    if r['task_log']:
                        try:
                            task_log = json.loads(r['task_log'])
                        except (json.JSONDecodeError, TypeError):
                            pass
                    blocks.append({
                        'type': 'task', 'task_id': r['task_id'],
                        'node_id': r['node_id'] or '',
                        'command': r['command'] or '',
                        'log': task_log,
                    })
                if r['results']:
                    blocks.append({'type': 'text', 'content': r['results']})
                if blocks:
                    conn.execute('UPDATE experiments SET blocks=? WHERE id=?',
                                 (json.dumps(blocks, ensure_ascii=False), r['id']))
            conn.commit()
            # 尝试删除旧列（SQLite 3.35+ 支持 DROP COLUMN）
            for col in ('description', 'results', 'node_id', 'task_id', 'command', 'task_log'):
                try:
                    conn.execute(f'ALTER TABLE experiments DROP COLUMN {col}')
                except sqlite3.OperationalError:
                    pass
        except sqlite3.OperationalError:
            pass  # 旧列不存在，无需迁移

        # 如果有旧 experiment_tasks 表，合并到对应实验的 blocks 中
        try:
            old_tasks = conn.execute(
                "SELECT experiment_id, task_id, node_id, command, task_log "
                "FROM experiment_tasks ORDER BY id ASC"
            ).fetchall()
            for t in old_tasks:
                exp = conn.execute(
                    "SELECT blocks FROM experiments WHERE id=?",
                    (t['experiment_id'],)).fetchone()
                if not exp:
                    continue
                blocks = json.loads(exp['blocks'] or '[]')
                task_log = None
                if t['task_log']:
                    try:
                        task_log = json.loads(t['task_log'])
                    except (json.JSONDecodeError, TypeError):
                        pass
                blocks.append({
                    'type': 'task', 'task_id': t['task_id'],
                    'node_id': t['node_id'] or '',
                    'command': t['command'] or '',
                    'log': task_log,
                })
                conn.execute('UPDATE experiments SET blocks=? WHERE id=?',
                             (json.dumps(blocks, ensure_ascii=False), t['experiment_id']))
            conn.execute('DROP TABLE IF EXISTS experiment_tasks')
            conn.commit()
        except sqlite3.OperationalError:
            pass

        # 迁移：添加 folder_id 列到 experiments
        try:
            conn.execute("ALTER TABLE experiments ADD COLUMN folder_id TEXT DEFAULT NULL "
                         "REFERENCES folders(id) ON DELETE SET NULL")
        except sqlite3.OperationalError:
            pass  # 列已存在

        # 迁移：创建 folder_id 索引（在添加列之后）
        try:
            conn.execute("CREATE INDEX IF NOT EXISTS idx_exp_folder ON experiments(folder_id)")
        except sqlite3.OperationalError:
            pass

        conn.commit()

    # ═══════════════════════════════════════════════════════════
    # Experiments CRUD
    # ═══════════════════════════════════════════════════════════

    def create_experiment(self, title: str, blocks: list = None,
                          tags: list = None, created_by: str = '',
                          folder_id: str = None, exp_id: str = None) -> str:
        conn = self._get_conn()
        exp_id = exp_id or uuid.uuid4().hex[:12]
        now = time.time()
        conn.execute(
            'INSERT INTO experiments (id, title, blocks, tags, created_by, folder_id, created_at, updated_at) '
            'VALUES (?, ?, ?, ?, ?, ?, ?, ?)',
            (exp_id, title,
             json.dumps(blocks or [], ensure_ascii=False),
             json.dumps(tags or [], ensure_ascii=False),
             created_by, folder_id, now, now))
        conn.commit()
        return exp_id

    def update_experiment(self, exp_id: str, **fields) -> bool:
        allowed = {'title', 'blocks', 'tags', 'folder_id'}
        conn = self._get_conn()
        updates = {}
        for k in allowed:
            if k in fields:
                val = fields[k]
                if k in ('blocks', 'tags') and isinstance(val, list):
                    val = json.dumps(val, ensure_ascii=False)
                updates[k] = val
        if not updates:
            return False
        updates['updated_at'] = time.time()
        set_clause = ', '.join(f'{k}=?' for k in updates)
        values = list(updates.values()) + [exp_id]
        conn.execute(f'UPDATE experiments SET {set_clause} WHERE id=?', values)
        conn.commit()
        return True

    def delete_experiment(self, exp_id: str) -> bool:
        conn = self._get_conn()
        cursor = conn.execute('DELETE FROM experiments WHERE id=?', (exp_id,))
        conn.commit()
        return cursor.rowcount > 0

    def get_experiment(self, exp_id: str) -> dict | None:
        conn = self._get_conn()
        row = conn.execute(
            'SELECT * FROM experiments WHERE id=?', (exp_id,)).fetchone()
        return self._row_to_dict(row) if row else None

    def list_experiments(self, search: str = '', tag: str = '',
                         created_by: str = '', folder_id: str = None,
                         limit: int = 100) -> list[dict]:
        conn = self._get_conn()
        conditions = []
        params = []
        if search:
            conditions.append('(title LIKE ? OR tags LIKE ? OR blocks LIKE ?)')
            like = f'%{search}%'
            params.extend([like, like, like])
        if tag:
            conditions.append('tags LIKE ?')
            params.append(f'%{tag}%')
        if created_by:
            conditions.append('created_by = ?')
            params.append(created_by)
        if folder_id is not None:
            # 包含子文件夹中的实验
            children = self._get_folder_descendants(folder_id)
            all_ids = [folder_id] + children
            placeholders = ','.join('?' for _ in all_ids)
            conditions.append(f'folder_id IN ({placeholders})')
            params.extend(all_ids)
        where = 'WHERE ' + ' AND '.join(conditions) if conditions else ''
        query = f'SELECT * FROM experiments {where} ORDER BY created_at DESC LIMIT ?'
        params.append(limit)
        return [self._row_to_dict(r) for r in conn.execute(query, params).fetchall()]

    @staticmethod
    def _row_to_dict(row: sqlite3.Row) -> dict:
        d = dict(row)
        for key in ('tags', 'blocks'):
            if key in d and isinstance(d[key], str):
                try:
                    d[key] = json.loads(d[key])
                except (json.JSONDecodeError, TypeError):
                    d[key] = [] if key == 'tags' else []
        return d

    # ═══════════════════════════════════════════════════════════
    # Folders CRUD
    # ═══════════════════════════════════════════════════════════

    def _get_folder_descendants(self, folder_id: str) -> list:
        """递归获取某文件夹下所有子文件夹 ID。"""
        conn = self._get_conn()
        result = []
        stack = [folder_id]
        while stack:
            rows = conn.execute(
                'SELECT id FROM folders WHERE parent_id IN ({})'.format(
                    ','.join('?' for _ in stack)),
                stack).fetchall()
            stack = [r['id'] for r in rows]
            result.extend(stack)
        return result

    def create_folder(self, name: str, parent_id: str = None) -> str:
        conn = self._get_conn()
        fid = uuid.uuid4().hex[:8]
        now = time.time()
        conn.execute(
            'INSERT INTO folders (id, name, parent_id, created_at) VALUES (?, ?, ?, ?)',
            (fid, name, parent_id, now))
        conn.commit()
        return fid

    def get_folder_tree(self) -> list:
        """返回所有文件夹列表，每个含 id/name/parent_id/children/exp_count。"""
        conn = self._get_conn()
        rows = conn.execute(
            'SELECT f.*, (SELECT COUNT(*) FROM experiments e '
            'WHERE e.folder_id = f.id) AS exp_count '
            'FROM folders f ORDER BY f.name ASC'
        ).fetchall()
        folders = [dict(r) for r in rows]
        # 构建树
        node_map = {f['id']: {**f, 'children': []} for f in folders}
        tree = []
        for f in folders:
            node = node_map[f['id']]
            if f['parent_id'] and f['parent_id'] in node_map:
                node_map[f['parent_id']]['children'].append(node)
            else:
                tree.append(node)
        # 递归聚合子文件夹的实验数
        def _agg(n):
            total = n.get('exp_count', 0) or 0
            for c in n['children']:
                total += _agg(c)
            n['total_exp_count'] = total
            return total
        for n in tree:
            _agg(n)
        return tree

    def rename_folder(self, fid: str, name: str) -> bool:
        conn = self._get_conn()
        conn.execute('UPDATE folders SET name=? WHERE id=?', (name, fid))
        conn.commit()
        return True

    def move_folder(self, fid: str, new_parent_id: str = None) -> bool:
        """移动文件夹到新父节点（不可是自己的子孙节点）。"""
        if new_parent_id and new_parent_id in self._get_folder_descendants(fid):
            return False  # 防止循环引用
        conn = self._get_conn()
        conn.execute('UPDATE folders SET parent_id=? WHERE id=?',
                     (new_parent_id, fid))
        conn.commit()
        return True

    def delete_folder(self, fid: str) -> bool:
        """删除文件夹：子文件夹上移，实验 folder_id 置空。"""
        conn = self._get_conn()
        folder = conn.execute('SELECT * FROM folders WHERE id=?', (fid,)).fetchone()
        if not folder:
            return False
        # 子文件夹上移
        conn.execute('UPDATE folders SET parent_id=? WHERE parent_id=?',
                     (folder['parent_id'], fid))
        # 实验移到父文件夹（或置空）
        conn.execute('UPDATE experiments SET folder_id=? WHERE folder_id=?',
                     (folder['parent_id'], fid))
        conn.execute('DELETE FROM folders WHERE id=?', (fid,))
        conn.commit()
        return True
