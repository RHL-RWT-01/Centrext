package sqlite

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	"centrext/internal/domain"
)

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("corrupt vector: %d bytes", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

const activeIn = `c.status = 'active' and (c.expires_at is null or c.expires_at > ?) and c.namespace_id in (`

func (t *txn) MissingEmbeddings(model string, namespaceIDs []string, notExpiredAt time.Time, limit int) ([]domain.EmbedJob, error) {
	if len(namespaceIDs) == 0 {
		return nil, nil
	}
	q := `select c.id, c.current_version, r.title, r.body
		from contexts c
		join revisions r on r.context_id = c.id and r.version = c.current_version
		left join embeddings e on e.context_id = c.id and e.model = ?
		where ` + activeIn + placeholders(len(namespaceIDs)) + `)
		and (e.context_id is null or e.version != c.current_version)
		order by c.updated_at desc limit ?`
	args := []any{model, ms(notExpiredAt)}
	for _, id := range namespaceIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EmbedJob
	for rows.Next() {
		var j domain.EmbedJob
		if err := rows.Scan(&j.ContextID, &j.Version, &j.Title, &j.Body); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (t *txn) PutEmbedding(contextID string, version int, model string, vector []float32, at time.Time) error {
	_, err := t.tx.Exec(`insert into embeddings(context_id, model, version, vector, created_at) values(?,?,?,?,?)
		on conflict(context_id, model) do update set version = excluded.version, vector = excluded.vector, created_at = excluded.created_at
		where excluded.version >= embeddings.version`,
		contextID, model, version, encodeVector(vector), ms(at))
	return err
}

func (t *txn) EmbeddedContexts(f domain.EmbeddingFilter) ([]domain.EmbeddedContext, error) {
	if len(f.NamespaceIDs) == 0 {
		return nil, nil
	}
	cols := "c." + strings.ReplaceAll(ctxCols, ", ", ", c.")
	q := `select ` + cols + `, e.vector
		from contexts c join embeddings e on e.context_id = c.id and e.model = ? and e.version = c.current_version
		where ` + activeIn + placeholders(len(f.NamespaceIDs)) + `)`
	args := []any{f.Model, ms(f.NotExpiredAt)}
	for _, id := range f.NamespaceIDs {
		args = append(args, id)
	}
	if len(f.Types) > 0 {
		q += ` and c.type in (` + placeholders(len(f.Types)) + `)`
		for _, ty := range f.Types {
			args = append(args, ty)
		}
	}
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EmbeddedContext
	for rows.Next() {
		var c domain.Context
		var reviewed int
		var exp sql.NullInt64
		var created, updated int64
		var blob []byte
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.Type, &c.Status, &c.CurrentVersion, &c.HeadTrust,
			&reviewed, &exp, &c.SupersededBy, &created, &updated, &blob); err != nil {
			return nil, err
		}
		c.Reviewed, c.ExpiresAt = reviewed == 1, fromNullMs(exp)
		c.CreatedAt, c.UpdatedAt = fromMs(created), fromMs(updated)
		v, err := decodeVector(blob)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.EmbeddedContext{Context: c, Vector: v})
	}
	return out, rows.Err()
}

func (t *txn) CountEmbeddings(model string, namespaceIDs []string, notExpiredAt time.Time) (int, int, error) {
	if len(namespaceIDs) == 0 {
		return 0, 0, nil
	}
	q := `select count(*), count(e.context_id)
		from contexts c left join embeddings e on e.context_id = c.id and e.model = ? and e.version = c.current_version
		where ` + activeIn + placeholders(len(namespaceIDs)) + `)`
	args := []any{model, ms(notExpiredAt)}
	for _, id := range namespaceIDs {
		args = append(args, id)
	}
	var active, embedded int
	err := t.tx.QueryRow(q, args...).Scan(&active, &embedded)
	return active, embedded, err
}
