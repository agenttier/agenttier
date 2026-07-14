/*
 * Copyright 2024 AgentTier Authors.
 * SPDX-License-Identifier: Apache-2.0
 */

import { useState, useEffect, useCallback } from 'react';
import { fetchTemplates, createTemplate, updateTemplate, deleteTemplate } from '../api/client';
import type { Template } from '../types';
import { dump as yamlDump, load as yamlLoad } from 'js-yaml';

function specToYaml(spec: any): string {
  if (!spec) return '';
  return yamlDump(spec, { indent: 2, lineWidth: 120, noRefs: true });
}

function yamlToSpec(text: string): any {
  return yamlLoad(text);
}

const STARTER_YAML = `description: My custom template
image:
  repository: ghcr.io/agenttier/sandbox-general:latest
resources:
  requests:
    cpu: 500m
    memory: 1Gi
  limits:
    cpu: "2"
    memory: 4Gi
storage:
  size: 10Gi
  mountPath: /workspace
network:
  allowInternet: true
harness:
  shell: /bin/bash
  workingDir: /workspace
  tools: []
timeout: 24h
idleTimeout: 4h
`;

export default function Templates() {
  const [templates, setTemplates] = useState<Template[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [editorContent, setEditorContent] = useState('');
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  const loadTemplates = useCallback(async () => {
    try {
      const list = await fetchTemplates();
      setTemplates(list);
    } catch (e) {
      setError('Failed to load templates');
    }
  }, []);

  useEffect(() => { loadTemplates(); }, [loadTemplates]);

  useEffect(() => {
    if (selected) {
      const t = templates.find(t => t.name === selected);
      if (t?.spec) {
        setEditorContent(specToYaml(t.spec));
      }
    }
  }, [selected, templates]);

  const handleSelect = (name: string) => {
    setCreating(false);
    setSelected(name);
    setError(null);
    setSuccess(null);
  };

  const handleCreate = () => {
    setSelected(null);
    setCreating(true);
    setNewName('');
    setEditorContent(STARTER_YAML);
    setError(null);
    setSuccess(null);
  };

  const handleSave = async () => {
    setError(null);
    setSuccess(null);
    setSaving(true);
    try {
      const spec = yamlToSpec(editorContent);
      if (creating) {
        if (!newName.trim()) { setError('Template name is required'); setSaving(false); return; }
        await createTemplate(newName.trim(), spec);
        setSuccess(`Template "${newName.trim()}" created`);
        setCreating(false);
        setSelected(newName.trim());
      } else if (selected) {
        await updateTemplate(selected, spec);
        setSuccess(`Template "${selected}" updated`);
      }
      await loadTemplates();
    } catch (e: any) {
      setError(e.message || 'Failed to save template');
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!selected) return;
    if (!confirm(`Delete template "${selected}"? This cannot be undone.`)) return;
    setError(null);
    try {
      await deleteTemplate(selected);
      setSelected(null);
      setEditorContent('');
      setSuccess(`Template "${selected}" deleted`);
      await loadTemplates();
    } catch (e: any) {
      setError(e.message || 'Failed to delete template');
    }
  };

  return (
    <div style={{ display: 'flex', height: '100%' }}>
      {/* Template list sidebar */}
      <div style={{ width: '240px', minWidth: '240px', borderRight: '1px solid #e5e4e7', padding: '20px 12px', overflowY: 'auto' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '16px', padding: '0 8px' }}>
          <h2 style={{ margin: 0, fontSize: '16px', color: '#08060d' }}>Templates</h2>
          <button onClick={handleCreate} title="Create template"
            style={{ background: '#aa3bff', color: '#fff', border: 'none', borderRadius: '6px', width: '28px', height: '28px', fontSize: '18px', cursor: 'pointer', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
            +
          </button>
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: '4px' }}>
          {templates.map(t => (
            <div key={t.name} onClick={() => handleSelect(t.name)}
              style={{
                padding: '10px 12px', borderRadius: '8px', cursor: 'pointer',
                background: selected === t.name ? 'rgba(170,59,255,0.08)' : 'transparent',
                border: selected === t.name ? '1px solid #aa3bff' : '1px solid transparent',
              }}>
              <div style={{ fontSize: '13px', fontWeight: 600, color: '#08060d' }}>{t.name}</div>
              <div style={{ fontSize: '11px', color: '#6b6375', marginTop: '2px' }}>{t.description || 'No description'}</div>
            </div>
          ))}
        </div>
      </div>

      {/* Editor area */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', padding: '20px' }}>
        {!selected && !creating && (
          <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#6b6375' }}>
            Select a template to edit, or click + to create a new one.
          </div>
        )}

        {(selected || creating) && (
          <>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '12px' }}>
              <div>
                {creating ? (
                  <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="template-name"
                    style={{ fontSize: '18px', fontWeight: 700, border: '1px solid #e5e4e7', borderRadius: '6px', padding: '4px 10px', width: '260px' }} />
                ) : (
                  <h2 style={{ margin: 0, fontSize: '18px', color: '#08060d' }}>{selected}</h2>
                )}
              </div>
              <div style={{ display: 'flex', gap: '8px' }}>
                {selected && (
                  <button onClick={handleDelete}
                    style={{ padding: '6px 14px', borderRadius: '6px', border: '1px solid #ef4444', background: 'transparent', color: '#ef4444', fontSize: '13px', cursor: 'pointer' }}>
                    Delete
                  </button>
                )}
                <button onClick={handleSave} disabled={saving}
                  style={{ padding: '6px 14px', borderRadius: '6px', border: 'none', background: saving ? '#d1d5db' : '#aa3bff', color: '#fff', fontSize: '13px', fontWeight: 600, cursor: saving ? 'not-allowed' : 'pointer' }}>
                  {saving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>

            {error && <div style={{ padding: '8px 12px', background: '#fef2f2', border: '1px solid #fecaca', borderRadius: '6px', color: '#dc2626', fontSize: '13px', marginBottom: '8px' }}>{error}</div>}
            {success && <div style={{ padding: '8px 12px', background: '#f0fdf4', border: '1px solid #bbf7d0', borderRadius: '6px', color: '#16a34a', fontSize: '13px', marginBottom: '8px' }}>{success}</div>}

            <div style={{ flex: 1, position: 'relative' }}>
              <textarea
                value={editorContent}
                onChange={e => setEditorContent(e.target.value)}
                spellCheck={false}
                style={{
                  width: '100%', height: '100%', fontFamily: 'JetBrains Mono, Menlo, Monaco, monospace',
                  fontSize: '13px', lineHeight: '1.5', padding: '16px', border: '1px solid #e5e4e7',
                  borderRadius: '8px', resize: 'none', background: '#fafafa', color: '#08060d',
                  boxSizing: 'border-box', outline: 'none', tabSize: 2,
                }}
              />
            </div>

            <div style={{ marginTop: '8px', fontSize: '11px', color: '#6b6375' }}>
              Edit the template spec in YAML. This defines what goes into sandboxes created from this template — image, tools, resources, startup scripts, etc.
            </div>
          </>
        )}
      </div>
    </div>
  );
}
