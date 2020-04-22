// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package span

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/constraint"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/errors"
)

// Builder is a single struct for generating key spans from Constraints, Datums and encDatums.
type Builder struct {
	table         *sqlbase.TableDescriptor
	index         *sqlbase.IndexDescriptor
	indexColTypes []types.T
	indexColDirs  []sqlbase.IndexDescriptor_Direction

	// KeyPrefix is the prefix of keys generated by the builder.
	KeyPrefix []byte
	alloc     sqlbase.DatumAlloc

	// TODO (rohany): The interstices are used to convert opt constraints into spans. In future work,
	//  we should unify the codepaths and use the allocation free method used on datums.
	//  This work is tracked in #42738.
	interstices [][]byte

	neededFamilies []sqlbase.FamilyID
}

// Use some functions that aren't needed right now to make the linter happy.
var _ = (*Builder).UnsetNeededColumns
var _ = (*Builder).SetNeededFamilies
var _ = (*Builder).UnsetNeededFamilies

// MakeBuilder creates a Builder for a table and index.
func MakeBuilder(table *sqlbase.TableDescriptor, index *sqlbase.IndexDescriptor) *Builder {
	s := &Builder{
		table:          table,
		index:          index,
		KeyPrefix:      sqlbase.MakeIndexKeyPrefix(table, index.ID),
		interstices:    make([][]byte, len(index.ColumnDirections)+len(index.ExtraColumnIDs)+1),
		neededFamilies: nil,
	}

	var columnIDs sqlbase.ColumnIDs
	columnIDs, s.indexColDirs = index.FullColumnIDs()
	s.indexColTypes = make([]types.T, len(columnIDs))
	for i, colID := range columnIDs {
		// TODO (rohany): do I need to look at table columns with mutations here as well?
		for _, col := range table.Columns {
			if col.ID == colID {
				s.indexColTypes[i] = col.Type
				break
			}
		}
	}

	// Set up the interstices for encoding interleaved tables later.
	s.interstices[0] = s.KeyPrefix
	if len(index.Interleave.Ancestors) > 0 {
		// TODO(rohany): too much of this code is copied from EncodePartialIndexKey.
		sharedPrefixLen := 0
		for i, ancestor := range index.Interleave.Ancestors {
			// The first ancestor is already encoded in interstices[0].
			if i != 0 {
				s.interstices[sharedPrefixLen] =
					encoding.EncodeUvarintAscending(s.interstices[sharedPrefixLen], uint64(ancestor.TableID))
				s.interstices[sharedPrefixLen] =
					encoding.EncodeUvarintAscending(s.interstices[sharedPrefixLen], uint64(ancestor.IndexID))
			}
			sharedPrefixLen += int(ancestor.SharedPrefixLen)
			s.interstices[sharedPrefixLen] = encoding.EncodeInterleavedSentinel(s.interstices[sharedPrefixLen])
		}
		s.interstices[sharedPrefixLen] =
			encoding.EncodeUvarintAscending(s.interstices[sharedPrefixLen], uint64(table.ID))
		s.interstices[sharedPrefixLen] =
			encoding.EncodeUvarintAscending(s.interstices[sharedPrefixLen], uint64(index.ID))
	}

	return s
}

// N.B. [Un]SetNeeded{Columns,Families} interact / overwrite each other.

// SetNeededColumns sets the needed columns on the Builder. This information
// is used by MaybeSplitSpanIntoSeparateFamilies.
func (s *Builder) SetNeededColumns(neededCols util.FastIntSet) {
	s.neededFamilies = sqlbase.NeededColumnFamilyIDs(neededCols, s.table, s.index)
}

// UnsetNeededColumns resets the needed columns for column family specific optimizations
// that the Builder performs.
func (s *Builder) UnsetNeededColumns() {
	s.neededFamilies = nil
}

// SetNeededFamilies sets the needed families of the span builder directly. This information
// is used by MaybeSplitSpanIntoSeparateFamilies.
func (s *Builder) SetNeededFamilies(neededFamilies []sqlbase.FamilyID) {
	s.neededFamilies = neededFamilies
}

// UnsetNeededFamilies resets the needed families for column family specific optimizations
// that the Builder performs.
func (s *Builder) UnsetNeededFamilies() {
	s.neededFamilies = nil
}

// SpanFromEncDatums encodes a span with prefixLen constraint columns from the index.
// SpanFromEncDatums assumes that the EncDatums in values are in the order of the index columns.
// It also returns whether or not the input values contain a null value or not, which can be
// used as input for CanSplitSpanIntoSeparateFamilies.
func (s *Builder) SpanFromEncDatums(
	values sqlbase.EncDatumRow, prefixLen int,
) (_ roachpb.Span, containsNull bool, _ error) {
	return sqlbase.MakeSpanFromEncDatums(
		values[:prefixLen], s.indexColTypes[:prefixLen], s.indexColDirs[:prefixLen], s.table, s.index, &s.alloc, s.KeyPrefix)
}

// SpanFromDatumRow generates an index span with prefixLen constraint columns from the index.
// SpanFromDatumRow assumes that values is a valid table row for the Builder's table.
// It also returns whether or not the input values contain a null value or not, which can be
// used as input for CanSplitSpanIntoSeparateFamilies.
func (s *Builder) SpanFromDatumRow(
	values tree.Datums, prefixLen int, colMap map[sqlbase.ColumnID]int,
) (_ roachpb.Span, containsNull bool, _ error) {
	return sqlbase.EncodePartialIndexSpan(s.table, s.index, prefixLen, colMap, values, s.KeyPrefix)
}

// SpanToPointSpan converts a span into a span that represents a point lookup on a
// specific family. It is up to the caller to ensure that this is a safe operation,
// by calling CanSplitSpanIntoSeparateFamilies before using it.
func (s *Builder) SpanToPointSpan(span roachpb.Span, family sqlbase.FamilyID) roachpb.Span {
	key := keys.MakeFamilyKey(span.Key, uint32(family))
	return roachpb.Span{Key: key, EndKey: roachpb.Key(key).PrefixEnd()}
}

// MaybeSplitSpanIntoSeparateFamilies uses the needed columns configured by
// SetNeededColumns to conditionally split the input span into multiple family
// specific spans. prefixLen is the number of index columns encoded in the span.
//
// The function accepts a slice of spans to append to.
func (s *Builder) MaybeSplitSpanIntoSeparateFamilies(
	appendTo roachpb.Spans, span roachpb.Span, prefixLen int, containsNull bool,
) roachpb.Spans {
	if s.neededFamilies != nil && s.CanSplitSpanIntoSeparateFamilies(len(s.neededFamilies), prefixLen, containsNull) {
		return sqlbase.SplitSpanIntoSeparateFamilies(appendTo, span, s.neededFamilies)
	}
	return append(appendTo, span)
}

// CanSplitSpanIntoSeparateFamilies returns whether a span encoded with prefixLen keys and numNeededFamilies
// needed families can be safely split into multiple family specific spans.
func (s *Builder) CanSplitSpanIntoSeparateFamilies(
	numNeededFamilies, prefixLen int, containsNull bool,
) bool {
	// We can only split a span into separate family specific point lookups if:
	// * We have a unique index.
	// * The index we are generating spans for actually has multiple families:
	//   - In the case of the primary index, that means the table itself has
	//     multiple families.
	//   - In the case of a secondary index, the table must have multiple families
	//     and the index must store some columns.
	// * If we have a secondary index, then containsNull must be false
	//   and it cannot be an inverted index.
	// * We have all of the lookup columns of the index.
	// * We don't need all of the families.
	return s.index.Unique && len(s.table.Families) > 1 &&
		(s.index.ID == s.table.PrimaryIndex.ID ||
			// Secondary index specific checks.
			(s.index.Version == sqlbase.SecondaryIndexFamilyFormatVersion &&
				!containsNull &&
				len(s.index.StoreColumnIDs) > 0 &&
				s.index.Type == sqlbase.IndexDescriptor_FORWARD)) &&
		prefixLen == len(s.index.ColumnIDs) &&
		numNeededFamilies < len(s.table.Families)
}

// Functions for optimizer related span generation are below.

// SpansFromConstraint generates spans from an optimizer constraint.
// TODO (rohany): In future work, there should be a single API to generate spans
//  from constraints, datums and encdatums.
func (s *Builder) SpansFromConstraint(
	c *constraint.Constraint, needed exec.TableColumnOrdinalSet, forDelete bool,
) (roachpb.Spans, error) {
	var spans roachpb.Spans
	var err error
	if c == nil || c.IsUnconstrained() {
		// Encode a full span.
		spans, err = s.appendSpansFromConstraintSpan(spans, &constraint.UnconstrainedSpan, needed, forDelete)
		if err != nil {
			return nil, err
		}
		return spans, nil
	}

	spans = make(roachpb.Spans, 0, c.Spans.Count())
	for i := 0; i < c.Spans.Count(); i++ {
		spans, err = s.appendSpansFromConstraintSpan(spans, c.Spans.Get(i), needed, forDelete)
		if err != nil {
			return nil, err
		}
	}
	return spans, nil
}

// UnconstrainedSpans returns the full span corresponding to the Builder's
// table and index.
func (s *Builder) UnconstrainedSpans(forDelete bool) (roachpb.Spans, error) {
	return s.SpansFromConstraint(nil, exec.TableColumnOrdinalSet{}, forDelete)
}

// appendSpansFromConstraintSpan converts a constraint.Span to one or more
// roachpb.Spans and appends them to the provided spans. It appends multiple
// spans in the case that multiple, non-adjacent column families should be
// scanned. The forDelete parameter indicates whether these spans will be used
// for row deletion.
func (s *Builder) appendSpansFromConstraintSpan(
	appendTo roachpb.Spans, cs *constraint.Span, needed exec.TableColumnOrdinalSet, forDelete bool,
) (roachpb.Spans, error) {
	var span roachpb.Span
	var err error
	var containsNull bool
	// Encode each logical part of the start key.
	span.Key, containsNull, err = s.encodeConstraintKey(cs.StartKey())
	if err != nil {
		return nil, err
	}
	if cs.StartBoundary() == constraint.IncludeBoundary {
		span.Key = append(span.Key, s.interstices[cs.StartKey().Length()]...)
	} else {
		// We need to exclude the value this logical part refers to.
		span.Key = span.Key.PrefixEnd()
	}
	// Encode each logical part of the end key.
	span.EndKey, _, err = s.encodeConstraintKey(cs.EndKey())
	if err != nil {
		return nil, err
	}
	span.EndKey = append(span.EndKey, s.interstices[cs.EndKey().Length()]...)

	// Optimization: for single row lookups on a table with multiple column
	// families, only scan the relevant column families. This is disabled for
	// deletions to ensure that the entire row is deleted.
	if !forDelete && needed.Len() > 0 && span.Key.Equal(span.EndKey) {
		neededFamilyIDs := sqlbase.NeededColumnFamilyIDs(needed, s.table, s.index)
		if s.CanSplitSpanIntoSeparateFamilies(len(neededFamilyIDs), cs.StartKey().Length(), containsNull) {
			return sqlbase.SplitSpanIntoSeparateFamilies(appendTo, span, neededFamilyIDs), nil
		}
	}

	// We tighten the end key to prevent reading interleaved children after the
	// last parent key. If cs.End.Inclusive is true, we also advance the key as
	// necessary.
	endInclusive := cs.EndBoundary() == constraint.IncludeBoundary
	span.EndKey, err = sqlbase.AdjustEndKeyForInterleave(s.table, s.index, span.EndKey, endInclusive)
	if err != nil {
		return nil, err
	}
	return append(appendTo, span), nil
}

// encodeConstraintKey encodes each logical part of a constraint.Key into a
// roachpb.Key; interstices[i] is inserted before the i-th value.
func (s *Builder) encodeConstraintKey(
	ck constraint.Key,
) (_ roachpb.Key, containsNull bool, _ error) {
	var key []byte
	for i := 0; i < ck.Length(); i++ {
		val := ck.Value(i)
		if val == tree.DNull {
			containsNull = true
		}
		key = append(key, s.interstices[i]...)

		var err error
		// For extra columns (like implicit columns), the direction
		// is ascending.
		dir := encoding.Ascending
		if i < len(s.index.ColumnDirections) {
			dir, err = s.index.ColumnDirections[i].ToEncodingDirection()
			if err != nil {
				return nil, false, err
			}
		}

		if s.index.Type == sqlbase.IndexDescriptor_INVERTED {
			keys, err := sqlbase.EncodeInvertedIndexTableKeys(val, key)
			if err != nil {
				return nil, false, err
			}
			if len(keys) == 0 {
				err := errors.AssertionFailedf("trying to use null key in index lookup")
				return nil, false, err
			}
			if len(keys) > 1 {
				err := errors.AssertionFailedf("trying to use multiple keys in index lookup")
				return nil, false, err
			}
			key = keys[0]
		} else {
			key, err = sqlbase.EncodeTableKey(key, val, dir)
			if err != nil {
				return nil, false, err
			}
		}
	}
	return key, containsNull, nil
}
