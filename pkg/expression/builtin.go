// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

//go:generate go run generator/compare_vec.go
//go:generate go run generator/control_vec.go
//go:generate go run generator/other_vec.go
//go:generate go run generator/string_vec.go
//go:generate go run generator/time_vec.go
//go:generate go run generator/builtin_threadsafe.go

package expression

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gogo/protobuf/proto"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/expression/expropt"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/charset"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/set"
	"github.com/pingcap/tipb/go-tipb"
)

// baseBuiltinFunc will be contained in every struct that implement builtinFunc interface.
type baseBuiltinFunc struct {
	bufAllocator columnBufferAllocator
	args         []Expression
	tp           *types.FieldType `plan-cache-clone:"shallow"`
	pbCode       tipb.ScalarFuncSig
	ctor         collate.Collator

	childrenVectorized     bool
	childrenVectorizedOnce *sync.Once

	safeToShareAcrossSessionFlag uint32 // 0 not-initialized, 1 safe, 2 unsafe

	collationInfo

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

// SafeToShareAcrossSession implements the builtinFunc interface.
func (*baseBuiltinFunc) SafeToShareAcrossSession() bool {
	return false
}

func (b *baseBuiltinFunc) PbCode() tipb.ScalarFuncSig {
	return b.pbCode
}

func (*baseBuiltinFunc) RequiredOptionalEvalProps() (set OptionalEvalPropKeySet) {
	return
}

// metadata returns the metadata of a function.
// metadata means some functions contain extra inner fields which will not
// contain in `tipb.Expr.children` but must be pushed down to coprocessor
func (*baseBuiltinFunc) metadata() proto.Message {
	// We will not use a field to store them because of only
	// a few functions contain implicit parameters
	return nil
}

func (b *baseBuiltinFunc) setPbCode(c tipb.ScalarFuncSig) {
	b.pbCode = c
}

func (b *baseBuiltinFunc) setCollator(ctor collate.Collator) {
	b.ctor = ctor
}

func (b *baseBuiltinFunc) collator() collate.Collator {
	return b.ctor
}

func adjustNullFlagForReturnType(ctx EvalContext, funcName string, args []Expression, bf baseBuiltinFunc) {
	if functionSetForReturnTypeAlwaysNotNull.Exist(funcName) {
		bf.tp.AddFlag(mysql.NotNullFlag)
	} else if functionSetForReturnTypeAlwaysNullable.Exist(funcName) {
		bf.tp.DelFlag(mysql.NotNullFlag)
	} else if functionSetForReturnTypeNotNullOnNotNull.Exist(funcName) {
		returnNullable := false
		for _, arg := range args {
			if !mysql.HasNotNullFlag(arg.GetType(ctx).GetFlag()) {
				returnNullable = true
				break
			}
		}
		if returnNullable {
			bf.tp.DelFlag(mysql.NotNullFlag)
		} else {
			bf.tp.AddFlag(mysql.NotNullFlag)
		}
	}
}

func newBaseBuiltinFunc(ctx BuildContext, funcName string, args []Expression, tp *types.FieldType) (baseBuiltinFunc, error) {
	if ctx == nil {
		return baseBuiltinFunc{}, errors.New("unexpected nil session ctx")
	}
	retType := tp.EvalType()
	ec, err := deriveCollation(ctx, funcName, args, retType, retType)
	if err != nil {
		return baseBuiltinFunc{}, err
	}

	bf := baseBuiltinFunc{
		bufAllocator:           newLocalColumnPool(),
		childrenVectorizedOnce: new(sync.Once),

		args: args,
		tp:   tp,
	}
	bf.SetCharsetAndCollation(ec.Charset, ec.Collation)
	bf.setCollator(collate.GetCollator(ec.Collation))
	bf.SetCoercibility(ec.Coer)
	bf.SetRepertoire(ec.Repe)
	adjustNullFlagForReturnType(ctx.GetEvalCtx(), funcName, args, bf)
	return bf, nil
}

func newReturnFieldTypeForBaseBuiltinFunc(funcName string, retType types.EvalType, ec *ExprCollation) *types.FieldType {
	var fieldType *types.FieldType
	switch retType {
	case types.ETInt:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeLonglong).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxIntWidth).BuildP()
	case types.ETReal:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeDouble).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxRealWidth).SetDecimal(types.UnspecifiedLength).BuildP()
	case types.ETDecimal:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeNewDecimal).SetFlag(mysql.BinaryFlag).SetFlen(11).BuildP()
	case types.ETString:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeVarString).SetFlen(types.UnspecifiedLength).SetDecimal(types.UnspecifiedLength).SetCharset(ec.Charset).SetCollate(ec.Collation).BuildP()
	case types.ETDatetime:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeDatetime).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxDatetimeWidthWithFsp).SetDecimal(types.MaxFsp).BuildP()
	case types.ETTimestamp:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeTimestamp).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxDatetimeWidthWithFsp).SetDecimal(types.MaxFsp).BuildP()
	case types.ETDuration:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeDuration).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxDurationWidthWithFsp).SetDecimal(types.MaxFsp).BuildP()
	case types.ETJson:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeJSON).SetFlag(mysql.BinaryFlag).SetFlen(mysql.MaxBlobWidth).SetCharset(mysql.DefaultCharset).SetCollate(mysql.DefaultCollationName).BuildP()
	case types.ETVectorFloat32:
		fieldType = types.NewFieldTypeBuilder().SetType(mysql.TypeTiDBVectorFloat32).SetFlag(mysql.BinaryFlag).SetFlen(types.UnspecifiedLength).BuildP()
	}
	if mysql.HasBinaryFlag(fieldType.GetFlag()) && fieldType.GetType() != mysql.TypeJSON {
		fieldType.SetCharset(charset.CharsetBin)
		fieldType.SetCollate(charset.CollationBin)
	}
	if _, ok := booleanFunctions[funcName]; ok {
		fieldType.AddFlag(mysql.IsBooleanFlag)
	}
	return fieldType
}

// newBaseBuiltinFuncWithTp creates a built-in function signature with specified types of arguments and the return type of the function.
// argTps indicates the types of the args, retType indicates the return type of the built-in function.
// Every built-in function needs to be determined argTps and retType when we create it.
func newBaseBuiltinFuncWithTp(ctx BuildContext, funcName string, args []Expression, retType types.EvalType, argTps ...types.EvalType) (bf baseBuiltinFunc, err error) {
	if len(args) != len(argTps) {
		panic("unexpected length of args and argTps")
	}
	if ctx == nil {
		return baseBuiltinFunc{}, errors.New("unexpected nil session ctx")
	}

	// derive collation information for string function, and we must do it
	// before doing implicit cast.
	ec, err := deriveCollation(ctx, funcName, args, retType, argTps...)
	if err != nil {
		return
	}

	for i := range args {
		switch argTps[i] {
		case types.ETInt:
			args[i] = WrapWithCastAsInt(ctx, args[i], nil)
		case types.ETReal:
			args[i] = WrapWithCastAsReal(ctx, args[i])
		case types.ETDecimal:
			args[i] = WrapWithCastAsDecimal(ctx, args[i])
		case types.ETString:
			args[i] = WrapWithCastAsString(ctx, args[i])
			args[i] = HandleBinaryLiteral(ctx, args[i], ec, funcName, false)
		case types.ETDatetime:
			args[i] = WrapWithCastAsTime(ctx, args[i], types.NewFieldType(mysql.TypeDatetime))
		case types.ETTimestamp:
			args[i] = WrapWithCastAsTime(ctx, args[i], types.NewFieldType(mysql.TypeTimestamp))
		case types.ETDuration:
			args[i] = WrapWithCastAsDuration(ctx, args[i])
		case types.ETJson:
			args[i] = WrapWithCastAsJSON(ctx, args[i])
		case types.ETVectorFloat32:
			args[i] = WrapWithCastAsVectorFloat32(ctx, args[i])
		default:
			return baseBuiltinFunc{}, errors.Errorf("%s is not supported", argTps[i])
		}
	}

	fieldType := newReturnFieldTypeForBaseBuiltinFunc(funcName, retType, ec)
	bf = baseBuiltinFunc{
		bufAllocator:           newLocalColumnPool(),
		childrenVectorizedOnce: new(sync.Once),

		args: args,
		tp:   fieldType,
	}
	bf.SetCharsetAndCollation(ec.Charset, ec.Collation)
	bf.setCollator(collate.GetCollator(ec.Collation))
	bf.SetCoercibility(ec.Coer)
	bf.SetRepertoire(ec.Repe)
	// note this function must be called after wrap cast function to the args
	adjustNullFlagForReturnType(ctx.GetEvalCtx(), funcName, args, bf)
	return bf, nil
}

// newBaseBuiltinFuncWithFieldTypes creates a built-in function signature with specified field types of arguments and the return type of the function.
// argTps indicates the field types of the args, retType indicates the return type of the built-in function.
// newBaseBuiltinFuncWithTp and newBaseBuiltinFuncWithFieldTypes are essentially the same, but newBaseBuiltinFuncWithFieldTypes uses FieldType to cast args.
// If there are specific requirements for decimal/datetime/timestamp, newBaseBuiltinFuncWithFieldTypes should be used, such as if,ifnull and casewhen.
func newBaseBuiltinFuncWithFieldTypes(ctx BuildContext, funcName string, args []Expression, retType types.EvalType, argTps ...*types.FieldType) (bf baseBuiltinFunc, err error) {
	if len(args) != len(argTps) {
		panic("unexpected length of args and argTps")
	}
	if ctx == nil {
		return baseBuiltinFunc{}, errors.New("unexpected nil session ctx")
	}

	// derive collation information for string function, and we must do it
	// before doing implicit cast.
	argEvalTps := make([]types.EvalType, 0, len(argTps))
	for i := range args {
		argEvalTps = append(argEvalTps, argTps[i].EvalType())
	}
	ec, err := deriveCollation(ctx, funcName, args, retType, argEvalTps...)
	if err != nil {
		return
	}

	for i := range args {
		switch argTps[i].EvalType() {
		case types.ETInt:
			args[i] = WrapWithCastAsInt(ctx, args[i], argTps[i])
		case types.ETReal:
			args[i] = WrapWithCastAsReal(ctx, args[i])
		case types.ETString:
			args[i] = WrapWithCastAsString(ctx, args[i])
			args[i] = HandleBinaryLiteral(ctx, args[i], ec, funcName, false)
		case types.ETJson:
			args[i] = WrapWithCastAsJSON(ctx, args[i])
		// https://github.com/pingcap/tidb/issues/44196
		// For decimal/datetime/timestamp/duration types, it is necessary to ensure that decimal are consistent with the output type,
		// so adding a cast function here.
		case types.ETDecimal, types.ETDatetime, types.ETTimestamp, types.ETDuration:
			if !args[i].GetType(ctx.GetEvalCtx()).Equal(argTps[i]) {
				args[i] = BuildCastFunction(ctx, args[i], argTps[i])
			}
		}
	}

	fieldType := newReturnFieldTypeForBaseBuiltinFunc(funcName, retType, ec)
	bf = baseBuiltinFunc{
		bufAllocator:           newLocalColumnPool(),
		childrenVectorizedOnce: new(sync.Once),

		args: args,
		tp:   fieldType,
	}
	bf.SetCharsetAndCollation(ec.Charset, ec.Collation)
	bf.setCollator(collate.GetCollator(ec.Collation))
	bf.SetCoercibility(ec.Coer)
	bf.SetRepertoire(ec.Repe)
	// note this function must be called after wrap cast function to the args
	adjustNullFlagForReturnType(ctx.GetEvalCtx(), funcName, args, bf)
	return bf, nil
}

// newBaseBuiltinFuncWithFieldType create BaseBuiltinFunc with FieldType charset and collation.
// do not check and compute collation.
func newBaseBuiltinFuncWithFieldType(tp *types.FieldType, args []Expression) (baseBuiltinFunc, error) {
	bf := baseBuiltinFunc{
		bufAllocator:           newLocalColumnPool(),
		childrenVectorizedOnce: new(sync.Once),

		args: args,
		tp:   tp,
	}
	bf.SetCharsetAndCollation(tp.GetCharset(), tp.GetCollate())
	bf.setCollator(collate.GetCollator(tp.GetCollate()))
	return bf, nil
}

func (b *baseBuiltinFunc) getArgs() []Expression {
	return b.args
}

func (*baseBuiltinFunc) vecEvalInt(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalInt() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalReal(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalReal() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalString(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalString() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalDecimal(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalDecimal() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalTime(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalTime() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalDuration(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalDuration() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalJSON(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalJSON() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vecEvalVectorFloat32(EvalContext, *chunk.Chunk, *chunk.Column) error {
	return errors.Errorf("baseBuiltinFunc.vecEvalVectorFloat32() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalInt(EvalContext, chunk.Row) (int64, bool, error) {
	return 0, false, errors.Errorf("baseBuiltinFunc.evalInt() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalReal(EvalContext, chunk.Row) (float64, bool, error) {
	return 0, false, errors.Errorf("baseBuiltinFunc.evalReal() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalString(EvalContext, chunk.Row) (string, bool, error) {
	return "", false, errors.Errorf("baseBuiltinFunc.evalString() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalDecimal(EvalContext, chunk.Row) (*types.MyDecimal, bool, error) {
	return nil, false, errors.Errorf("baseBuiltinFunc.evalDecimal() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalTime(EvalContext, chunk.Row) (types.Time, bool, error) {
	return types.ZeroTime, false, errors.Errorf("baseBuiltinFunc.evalTime() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalDuration(EvalContext, chunk.Row) (types.Duration, bool, error) {
	return types.Duration{}, false, errors.Errorf("baseBuiltinFunc.evalDuration() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalJSON(EvalContext, chunk.Row) (types.BinaryJSON, bool, error) {
	return types.BinaryJSON{}, false, errors.Errorf("baseBuiltinFunc.evalJSON() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) evalVectorFloat32(EvalContext, chunk.Row) (types.VectorFloat32, bool, error) {
	return types.ZeroVectorFloat32, false, errors.Errorf("baseBuiltinFunc.evalVectorFloat32() should never be called, please contact the TiDB team for help")
}

func (*baseBuiltinFunc) vectorized() bool {
	return false
}

func (b *baseBuiltinFunc) isChildrenVectorized() bool {
	b.childrenVectorizedOnce.Do(func() {
		b.childrenVectorized = true
		for _, arg := range b.args {
			if !arg.Vectorized() {
				b.childrenVectorized = false
				break
			}
		}
	})
	return b.childrenVectorized
}

func (b *baseBuiltinFunc) getRetTp() *types.FieldType {
	if b.tp.EvalType() == types.ETString {
		if b.tp.GetFlen() >= mysql.MaxBlobWidth {
			b.tp.SetType(mysql.TypeLongBlob)
		} else if b.tp.GetFlen() >= 65536 {
			b.tp.SetType(mysql.TypeMediumBlob)
		}
		if len(b.tp.GetCharset()) <= 0 {
			charset, collate := charset.GetDefaultCharsetAndCollate()
			b.tp.SetCharset(charset)
			b.tp.SetCollate(collate)
		}
	}
	return b.tp
}

func (b *baseBuiltinFunc) equal(ctx EvalContext, fun builtinFunc) bool {
	funArgs := fun.getArgs()
	if len(funArgs) != len(b.args) {
		return false
	}
	for i := range b.args {
		if !b.args[i].Equal(ctx, funArgs[i]) {
			return false
		}
	}
	return true
}

func (b *baseBuiltinFunc) cloneFrom(from *baseBuiltinFunc) {
	b.args = make([]Expression, 0, len(from.args))
	for _, arg := range from.args {
		b.args = append(b.args, arg.Clone())
	}
	b.tp = from.tp
	b.pbCode = from.pbCode
	b.bufAllocator = newLocalColumnPool()
	b.childrenVectorizedOnce = new(sync.Once)
	if from.ctor != nil {
		b.ctor = from.ctor.Clone()
	}
}

func (*baseBuiltinFunc) Clone() builtinFunc {
	panic("you should not call this method.")
}

// baseBuiltinCastFunc will be contained in every struct that implement cast builtinFunc.
type baseBuiltinCastFunc struct {
	baseBuiltinFunc

	// inUnion indicates whether cast is in union context.
	inUnion bool
}

// metadata returns the metadata of cast functions
func (b *baseBuiltinCastFunc) metadata() proto.Message {
	args := &tipb.InUnionMetadata{
		InUnion: b.inUnion,
	}
	return args
}

func (b *baseBuiltinCastFunc) cloneFrom(from *baseBuiltinCastFunc) {
	b.baseBuiltinFunc.cloneFrom(&from.baseBuiltinFunc)
	b.inUnion = from.inUnion
}

func newBaseBuiltinCastFunc(builtinFunc baseBuiltinFunc, inUnion bool) baseBuiltinCastFunc {
	return baseBuiltinCastFunc{
		baseBuiltinFunc: builtinFunc,
		inUnion:         inUnion,
	}
}

func newBaseBuiltinCastFunc4String(ctx BuildContext, funcName string, args []Expression, tp *types.FieldType, isExplicitCharset bool) (baseBuiltinFunc, error) {
	var bf baseBuiltinFunc
	var err error
	if isExplicitCharset {
		bf = baseBuiltinFunc{
			bufAllocator:           newLocalColumnPool(),
			childrenVectorizedOnce: new(sync.Once),

			args: args,
			tp:   tp,
		}
		bf.SetCharsetAndCollation(tp.GetCharset(), tp.GetCollate())
		bf.setCollator(collate.GetCollator(tp.GetCollate()))
		bf.SetCoercibility(CoercibilityExplicit)
		bf.SetExplicitCharset(true)
		if tp.GetCharset() == charset.CharsetASCII {
			bf.SetRepertoire(ASCII)
		} else {
			bf.SetRepertoire(UNICODE)
		}
	} else {
		bf, err = newBaseBuiltinFunc(ctx, funcName, args, tp)
		if err != nil {
			return baseBuiltinFunc{}, err
		}
	}
	return bf, nil
}

// vecBuiltinFunc contains all vectorized methods for a builtin function.
type vecBuiltinFunc interface {
	// vectorized returns if this builtin function itself supports vectorized evaluation.
	vectorized() bool

	// isChildrenVectorized returns if its all children support vectorized evaluation.
	isChildrenVectorized() bool

	// vecEvalInt evaluates this builtin function in a vectorized manner.
	vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalReal evaluates this builtin function in a vectorized manner.
	vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalString evaluates this builtin function in a vectorized manner.
	vecEvalString(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalDecimal evaluates this builtin function in a vectorized manner.
	vecEvalDecimal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalTime evaluates this builtin function in a vectorized manner.
	vecEvalTime(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalDuration evaluates this builtin function in a vectorized manner.
	vecEvalDuration(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalJSON evaluates this builtin function in a vectorized manner.
	vecEvalJSON(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error

	// vecEvalVectorFloat32 evaluates this builtin function in a vectorized manner.
	vecEvalVectorFloat32(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error
}

// builtinFunc stands for a particular function signature.
type builtinFunc interface {
	expropt.RequireOptionalEvalProps
	vecBuiltinFunc
	SafeToShareAcrossSession

	// evalInt evaluates int result of builtinFunc by given row.
	evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error)
	// evalReal evaluates real representation of builtinFunc by given row.
	evalReal(ctx EvalContext, row chunk.Row) (val float64, isNull bool, err error)
	// evalString evaluates string representation of builtinFunc by given row.
	evalString(ctx EvalContext, row chunk.Row) (val string, isNull bool, err error)
	// evalDecimal evaluates decimal representation of builtinFunc by given row.
	evalDecimal(ctx EvalContext, row chunk.Row) (val *types.MyDecimal, isNull bool, err error)
	// evalTime evaluates DATE/DATETIME/TIMESTAMP representation of builtinFunc by given row.
	evalTime(ctx EvalContext, row chunk.Row) (val types.Time, isNull bool, err error)
	// evalDuration evaluates duration representation of builtinFunc by given row.
	evalDuration(ctx EvalContext, row chunk.Row) (val types.Duration, isNull bool, err error)
	// evalJSON evaluates JSON representation of builtinFunc by given row.
	evalJSON(ctx EvalContext, row chunk.Row) (val types.BinaryJSON, isNull bool, err error)
	evalVectorFloat32(ctx EvalContext, row chunk.Row) (val types.VectorFloat32, isNull bool, err error)
	// getArgs returns the arguments expressions.
	getArgs() []Expression
	// equal check if this function equals to another function.
	equal(EvalContext, builtinFunc) bool
	// getRetTp returns the return type of the built-in function.
	getRetTp() *types.FieldType
	// setPbCode sets pbCode for signature.
	setPbCode(tipb.ScalarFuncSig)
	// PbCode returns PbCode of this signature.
	PbCode() tipb.ScalarFuncSig
	// setCollator sets collator for signature.
	setCollator(ctor collate.Collator)
	// collator returns collator of this signature.
	collator() collate.Collator
	// metadata returns the metadata of a function.
	// metadata means some functions contain extra inner fields which will not
	// contain in `tipb.Expr.children` but must be pushed down to coprocessor
	metadata() proto.Message
	// Clone returns a copy of itself.
	Clone() builtinFunc

	MemoryUsage() int64

	CollationInfo
}

// baseFunctionClass will be contained in every struct that implement functionClass interface.
type baseFunctionClass struct {
	funcName string
	minArgs  int
	maxArgs  int
}

func (b *baseFunctionClass) verifyArgs(args []Expression) error {
	return b.verifyArgsByCount(len(args))
}

func (b *baseFunctionClass) verifyArgsByCount(l int) error {
	if l < b.minArgs || (b.maxArgs != -1 && l > b.maxArgs) {
		return ErrIncorrectParameterCount.GenWithStackByArgs(b.funcName)
	}
	return nil
}

// VerifyArgsWrapper verifies a function by its name and the count of the arguments.
// Note that this function assumes that the function is supported.
func VerifyArgsWrapper(name string, l int) error {
	f, ok := funcs[name]
	if !ok {
		return nil
	}
	return f.verifyArgsByCount(l)
}

// functionClass is the interface for a function which may contains multiple functions.
type functionClass interface {
	// getFunction gets a function signature by the types and the counts of given arguments.
	getFunction(ctx BuildContext, args []Expression) (builtinFunc, error)
	// verifyArgsByCount verifies the count of parameters.
	verifyArgsByCount(l int) error
}

// functionClassWithName is the interface for a function with a display name extended from functionClass
type functionClassWithName interface {
	functionClass

	// getDisplayName gets the display name of a function
	getDisplayName() string
}

// functions that always return not null type
// todo add more functions to this set
var functionSetForReturnTypeAlwaysNotNull = set.StringSet{
	ast.IsNull: {},
	ast.NullEQ: {},
}

// functions that always return nullable type
// todo add more functions to this set
var functionSetForReturnTypeAlwaysNullable = set.StringSet{
	ast.Div:    {}, // divide by zero
	ast.IntDiv: {}, // divide by zero
	ast.Mod:    {}, // mod by zero
}

// functions that if all the inputs are not null, then the result is not null
// Although most of the functions should belong to this, there may be some unknown
// bugs in TiDB that makes it not true in TiDB's implementation, so only add a few
// this time. The others can be added to the set case by case in the future
// todo add more functions to this set
var functionSetForReturnTypeNotNullOnNotNull = set.StringSet{
	ast.Plus:  {},
	ast.Minus: {},
	ast.Mul:   {},
}

// funcs holds all registered builtin functions. When new function is added,
// check expression/function_traits.go to see if it should be appended to
// any set there.
var funcs = map[string]functionClass{
	// common functions
	ast.Coalesce: &coalesceFunctionClass{baseFunctionClass{ast.Coalesce, 1, -1}},
	ast.IsNull:   &isNullFunctionClass{baseFunctionClass{ast.IsNull, 1, 1}},
	ast.Greatest: &greatestFunctionClass{baseFunctionClass{ast.Greatest, 2, -1}},
	ast.Least:    &leastFunctionClass{baseFunctionClass{ast.Least, 2, -1}},
	ast.Interval: &intervalFunctionClass{baseFunctionClass{ast.Interval, 2, -1}},

	// math functions
	ast.Abs:      &absFunctionClass{baseFunctionClass{ast.Abs, 1, 1}},
	ast.Acos:     &acosFunctionClass{baseFunctionClass{ast.Acos, 1, 1}},
	ast.Asin:     &asinFunctionClass{baseFunctionClass{ast.Asin, 1, 1}},
	ast.Atan:     &atanFunctionClass{baseFunctionClass{ast.Atan, 1, 2}},
	ast.Atan2:    &atanFunctionClass{baseFunctionClass{ast.Atan2, 2, 2}},
	ast.Ceil:     &ceilFunctionClass{baseFunctionClass{ast.Ceil, 1, 1}},
	ast.Ceiling:  &ceilFunctionClass{baseFunctionClass{ast.Ceiling, 1, 1}},
	ast.Conv:     &convFunctionClass{baseFunctionClass{ast.Conv, 3, 3}},
	ast.Cos:      &cosFunctionClass{baseFunctionClass{ast.Cos, 1, 1}},
	ast.Cot:      &cotFunctionClass{baseFunctionClass{ast.Cot, 1, 1}},
	ast.CRC32:    &crc32FunctionClass{baseFunctionClass{ast.CRC32, 1, 1}},
	ast.Degrees:  &degreesFunctionClass{baseFunctionClass{ast.Degrees, 1, 1}},
	ast.Exp:      &expFunctionClass{baseFunctionClass{ast.Exp, 1, 1}},
	ast.Floor:    &floorFunctionClass{baseFunctionClass{ast.Floor, 1, 1}},
	ast.Ln:       &logFunctionClass{baseFunctionClass{ast.Ln, 1, 1}},
	ast.Log:      &logFunctionClass{baseFunctionClass{ast.Log, 1, 2}},
	ast.Log2:     &log2FunctionClass{baseFunctionClass{ast.Log2, 1, 1}},
	ast.Log10:    &log10FunctionClass{baseFunctionClass{ast.Log10, 1, 1}},
	ast.PI:       &piFunctionClass{baseFunctionClass{ast.PI, 0, 0}},
	ast.Pow:      &powFunctionClass{baseFunctionClass{ast.Pow, 2, 2}},
	ast.Power:    &powFunctionClass{baseFunctionClass{ast.Power, 2, 2}},
	ast.Radians:  &radiansFunctionClass{baseFunctionClass{ast.Radians, 1, 1}},
	ast.Rand:     &randFunctionClass{baseFunctionClass{ast.Rand, 0, 1}},
	ast.Round:    &roundFunctionClass{baseFunctionClass{ast.Round, 1, 2}},
	ast.Sign:     &signFunctionClass{baseFunctionClass{ast.Sign, 1, 1}},
	ast.Sin:      &sinFunctionClass{baseFunctionClass{ast.Sin, 1, 1}},
	ast.Sqrt:     &sqrtFunctionClass{baseFunctionClass{ast.Sqrt, 1, 1}},
	ast.Tan:      &tanFunctionClass{baseFunctionClass{ast.Tan, 1, 1}},
	ast.Truncate: &truncateFunctionClass{baseFunctionClass{ast.Truncate, 2, 2}},

	// time functions
	ast.AddDate:          &addSubDateFunctionClass{baseFunctionClass{ast.AddDate, 3, 3}, addTime, addDuration, setAdd},
	ast.DateAdd:          &addSubDateFunctionClass{baseFunctionClass{ast.DateAdd, 3, 3}, addTime, addDuration, setAdd},
	ast.SubDate:          &addSubDateFunctionClass{baseFunctionClass{ast.SubDate, 3, 3}, subTime, subDuration, setSub},
	ast.DateSub:          &addSubDateFunctionClass{baseFunctionClass{ast.DateSub, 3, 3}, subTime, subDuration, setSub},
	ast.AddTime:          &addTimeFunctionClass{baseFunctionClass{ast.AddTime, 2, 2}},
	ast.ConvertTz:        &convertTzFunctionClass{baseFunctionClass{ast.ConvertTz, 3, 3}},
	ast.Curdate:          &currentDateFunctionClass{baseFunctionClass{ast.Curdate, 0, 0}},
	ast.CurrentDate:      &currentDateFunctionClass{baseFunctionClass{ast.CurrentDate, 0, 0}},
	ast.CurrentTime:      &currentTimeFunctionClass{baseFunctionClass{ast.CurrentTime, 0, 1}},
	ast.CurrentTimestamp: &nowFunctionClass{baseFunctionClass{ast.CurrentTimestamp, 0, 1}},
	ast.Curtime:          &currentTimeFunctionClass{baseFunctionClass{ast.Curtime, 0, 1}},
	ast.Date:             &dateFunctionClass{baseFunctionClass{ast.Date, 1, 1}},
	ast.DateLiteral:      &dateLiteralFunctionClass{baseFunctionClass{ast.DateLiteral, 1, 1}},
	ast.DateFormat:       &dateFormatFunctionClass{baseFunctionClass{ast.DateFormat, 2, 2}},
	ast.DateDiff:         &dateDiffFunctionClass{baseFunctionClass{ast.DateDiff, 2, 2}},
	ast.Day:              &dayOfMonthFunctionClass{baseFunctionClass{ast.Day, 1, 1}},
	ast.DayName:          &dayNameFunctionClass{baseFunctionClass{ast.DayName, 1, 1}},
	ast.DayOfMonth:       &dayOfMonthFunctionClass{baseFunctionClass{ast.DayOfMonth, 1, 1}},
	ast.DayOfWeek:        &dayOfWeekFunctionClass{baseFunctionClass{ast.DayOfWeek, 1, 1}},
	ast.DayOfYear:        &dayOfYearFunctionClass{baseFunctionClass{ast.DayOfYear, 1, 1}},
	ast.Extract:          &extractFunctionClass{baseFunctionClass{ast.Extract, 2, 2}},
	ast.FromDays:         &fromDaysFunctionClass{baseFunctionClass{ast.FromDays, 1, 1}},
	ast.FromUnixTime:     &fromUnixTimeFunctionClass{baseFunctionClass{ast.FromUnixTime, 1, 2}},
	ast.GetFormat:        &getFormatFunctionClass{baseFunctionClass{ast.GetFormat, 2, 2}},
	ast.Hour:             &hourFunctionClass{baseFunctionClass{ast.Hour, 1, 1}},
	ast.LocalTime:        &nowFunctionClass{baseFunctionClass{ast.LocalTime, 0, 1}},
	ast.LocalTimestamp:   &nowFunctionClass{baseFunctionClass{ast.LocalTimestamp, 0, 1}},
	ast.MakeDate:         &makeDateFunctionClass{baseFunctionClass{ast.MakeDate, 2, 2}},
	ast.MakeTime:         &makeTimeFunctionClass{baseFunctionClass{ast.MakeTime, 3, 3}},
	ast.MicroSecond:      &microSecondFunctionClass{baseFunctionClass{ast.MicroSecond, 1, 1}},
	ast.Minute:           &minuteFunctionClass{baseFunctionClass{ast.Minute, 1, 1}},
	ast.Month:            &monthFunctionClass{baseFunctionClass{ast.Month, 1, 1}},
	ast.MonthName:        &monthNameFunctionClass{baseFunctionClass{ast.MonthName, 1, 1}},
	ast.Now:              &nowFunctionClass{baseFunctionClass{ast.Now, 0, 1}},
	ast.PeriodAdd:        &periodAddFunctionClass{baseFunctionClass{ast.PeriodAdd, 2, 2}},
	ast.PeriodDiff:       &periodDiffFunctionClass{baseFunctionClass{ast.PeriodDiff, 2, 2}},
	ast.Quarter:          &quarterFunctionClass{baseFunctionClass{ast.Quarter, 1, 1}},
	ast.SecToTime:        &secToTimeFunctionClass{baseFunctionClass{ast.SecToTime, 1, 1}},
	ast.Second:           &secondFunctionClass{baseFunctionClass{ast.Second, 1, 1}},
	ast.StrToDate:        &strToDateFunctionClass{baseFunctionClass{ast.StrToDate, 2, 2}},
	ast.SubTime:          &subTimeFunctionClass{baseFunctionClass{ast.SubTime, 2, 2}},
	ast.Sysdate:          &sysDateFunctionClass{baseFunctionClass{ast.Sysdate, 0, 1}},
	ast.Time:             &timeFunctionClass{baseFunctionClass{ast.Time, 1, 1}},
	ast.TimeLiteral:      &timeLiteralFunctionClass{baseFunctionClass{ast.TimeLiteral, 1, 1}},
	ast.TimeFormat:       &timeFormatFunctionClass{baseFunctionClass{ast.TimeFormat, 2, 2}},
	ast.TimeToSec:        &timeToSecFunctionClass{baseFunctionClass{ast.TimeToSec, 1, 1}},
	ast.TimeDiff:         &timeDiffFunctionClass{baseFunctionClass{ast.TimeDiff, 2, 2}},
	ast.Timestamp:        &timestampFunctionClass{baseFunctionClass{ast.Timestamp, 1, 2}},
	ast.TimestampLiteral: &timestampLiteralFunctionClass{baseFunctionClass{ast.TimestampLiteral, 1, 2}},
	ast.TimestampAdd:     &timestampAddFunctionClass{baseFunctionClass{ast.TimestampAdd, 3, 3}},
	ast.TimestampDiff:    &timestampDiffFunctionClass{baseFunctionClass{ast.TimestampDiff, 3, 3}},
	ast.ToDays:           &toDaysFunctionClass{baseFunctionClass{ast.ToDays, 1, 1}},
	ast.ToSeconds:        &toSecondsFunctionClass{baseFunctionClass{ast.ToSeconds, 1, 1}},
	ast.UnixTimestamp:    &unixTimestampFunctionClass{baseFunctionClass{ast.UnixTimestamp, 0, 1}},
	ast.UTCDate:          &utcDateFunctionClass{baseFunctionClass{ast.UTCDate, 0, 0}},
	ast.UTCTime:          &utcTimeFunctionClass{baseFunctionClass{ast.UTCTime, 0, 1}},
	ast.UTCTimestamp:     &utcTimestampFunctionClass{baseFunctionClass{ast.UTCTimestamp, 0, 1}},
	ast.Week:             &weekFunctionClass{baseFunctionClass{ast.Week, 1, 2}},
	ast.Weekday:          &weekDayFunctionClass{baseFunctionClass{ast.Weekday, 1, 1}},
	ast.WeekOfYear:       &weekOfYearFunctionClass{baseFunctionClass{ast.WeekOfYear, 1, 1}},
	ast.Year:             &yearFunctionClass{baseFunctionClass{ast.Year, 1, 1}},
	ast.YearWeek:         &yearWeekFunctionClass{baseFunctionClass{ast.YearWeek, 1, 2}},
	ast.LastDay:          &lastDayFunctionClass{baseFunctionClass{ast.LastDay, 1, 1}},
	// TSO functions
	ast.TiDBBoundedStaleness: &tidbBoundedStalenessFunctionClass{baseFunctionClass{ast.TiDBBoundedStaleness, 2, 2}},
	ast.TiDBParseTso:         &tidbParseTsoFunctionClass{baseFunctionClass{ast.TiDBParseTso, 1, 1}},
	ast.TiDBParseTsoLogical:  &tidbParseTsoLogicalFunctionClass{baseFunctionClass{ast.TiDBParseTsoLogical, 1, 1}},
	ast.TiDBCurrentTso:       &tidbCurrentTsoFunctionClass{baseFunctionClass{ast.TiDBCurrentTso, 0, 0}},

	// string functions
	ast.ASCII:           &asciiFunctionClass{baseFunctionClass{ast.ASCII, 1, 1}},
	ast.Bin:             &binFunctionClass{baseFunctionClass{ast.Bin, 1, 1}},
	ast.BitLength:       &bitLengthFunctionClass{baseFunctionClass{ast.BitLength, 1, 1}},
	ast.CharFunc:        &charFunctionClass{baseFunctionClass{ast.CharFunc, 2, -1}},
	ast.CharLength:      &charLengthFunctionClass{baseFunctionClass{ast.CharLength, 1, 1}},
	ast.CharacterLength: &charLengthFunctionClass{baseFunctionClass{ast.CharacterLength, 1, 1}},
	ast.Concat:          &concatFunctionClass{baseFunctionClass{ast.Concat, 1, -1}},
	ast.ConcatWS:        &concatWSFunctionClass{baseFunctionClass{ast.ConcatWS, 2, -1}},
	ast.Convert:         &convertFunctionClass{baseFunctionClass{ast.Convert, 2, 2}},
	ast.Elt:             &eltFunctionClass{baseFunctionClass{ast.Elt, 2, -1}},
	ast.ExportSet:       &exportSetFunctionClass{baseFunctionClass{ast.ExportSet, 3, 5}},
	ast.Field:           &fieldFunctionClass{baseFunctionClass{ast.Field, 2, -1}},
	ast.Format:          &formatFunctionClass{baseFunctionClass{ast.Format, 2, 3}},
	ast.FromBase64:      &fromBase64FunctionClass{baseFunctionClass{ast.FromBase64, 1, 1}},
	ast.FindInSet:       &findInSetFunctionClass{baseFunctionClass{ast.FindInSet, 2, 2}},
	ast.Hex:             &hexFunctionClass{baseFunctionClass{ast.Hex, 1, 1}},
	ast.InsertFunc:      &insertFunctionClass{baseFunctionClass{ast.InsertFunc, 4, 4}},
	ast.Instr:           &instrFunctionClass{baseFunctionClass{ast.Instr, 2, 2}},
	ast.Lcase:           &lowerFunctionClass{baseFunctionClass{ast.Lcase, 1, 1}},
	ast.Left:            &leftFunctionClass{baseFunctionClass{ast.Left, 2, 2}},
	ast.Length:          &lengthFunctionClass{baseFunctionClass{ast.Length, 1, 1}},
	ast.LoadFile:        &loadFileFunctionClass{baseFunctionClass{ast.LoadFile, 1, 1}},
	ast.Locate:          &locateFunctionClass{baseFunctionClass{ast.Locate, 2, 3}},
	ast.Lower:           &lowerFunctionClass{baseFunctionClass{ast.Lower, 1, 1}},
	ast.Lpad:            &lpadFunctionClass{baseFunctionClass{ast.Lpad, 3, 3}},
	ast.LTrim:           &lTrimFunctionClass{baseFunctionClass{ast.LTrim, 1, 1}},
	ast.Mid:             &substringFunctionClass{baseFunctionClass{ast.Mid, 2, 3}},
	ast.MakeSet:         &makeSetFunctionClass{baseFunctionClass{ast.MakeSet, 2, -1}},
	ast.Oct:             &octFunctionClass{baseFunctionClass{ast.Oct, 1, 1}},
	ast.OctetLength:     &lengthFunctionClass{baseFunctionClass{ast.OctetLength, 1, 1}},
	ast.Ord:             &ordFunctionClass{baseFunctionClass{ast.Ord, 1, 1}},
	ast.Position:        &locateFunctionClass{baseFunctionClass{ast.Position, 2, 2}},
	ast.Quote:           &quoteFunctionClass{baseFunctionClass{ast.Quote, 1, 1}},
	ast.Repeat:          &repeatFunctionClass{baseFunctionClass{ast.Repeat, 2, 2}},
	ast.Replace:         &replaceFunctionClass{baseFunctionClass{ast.Replace, 3, 3}},
	ast.Reverse:         &reverseFunctionClass{baseFunctionClass{ast.Reverse, 1, 1}},
	ast.Right:           &rightFunctionClass{baseFunctionClass{ast.Right, 2, 2}},
	ast.RTrim:           &rTrimFunctionClass{baseFunctionClass{ast.RTrim, 1, 1}},
	ast.Rpad:            &rpadFunctionClass{baseFunctionClass{ast.Rpad, 3, 3}},
	ast.Space:           &spaceFunctionClass{baseFunctionClass{ast.Space, 1, 1}},
	ast.Strcmp:          &strcmpFunctionClass{baseFunctionClass{ast.Strcmp, 2, 2}},
	ast.Substring:       &substringFunctionClass{baseFunctionClass{ast.Substring, 2, 3}},
	ast.Substr:          &substringFunctionClass{baseFunctionClass{ast.Substr, 2, 3}},
	ast.SubstringIndex:  &substringIndexFunctionClass{baseFunctionClass{ast.SubstringIndex, 3, 3}},
	ast.ToBase64:        &toBase64FunctionClass{baseFunctionClass{ast.ToBase64, 1, 1}},
	ast.Trim:            &trimFunctionClass{baseFunctionClass{ast.Trim, 1, 3}},
	ast.Translate:       &translateFunctionClass{baseFunctionClass{ast.Translate, 3, 3}},
	ast.Upper:           &upperFunctionClass{baseFunctionClass{ast.Upper, 1, 1}},
	ast.Ucase:           &upperFunctionClass{baseFunctionClass{ast.Ucase, 1, 1}},
	ast.Unhex:           &unhexFunctionClass{baseFunctionClass{ast.Unhex, 1, 1}},
	ast.WeightString:    &weightStringFunctionClass{baseFunctionClass{ast.WeightString, 1, 3}},

	// information functions
	ast.ConnectionID:         &connectionIDFunctionClass{baseFunctionClass{ast.ConnectionID, 0, 0}},
	ast.CurrentUser:          &currentUserFunctionClass{baseFunctionClass{ast.CurrentUser, 0, 0}},
	ast.CurrentRole:          &currentRoleFunctionClass{baseFunctionClass{ast.CurrentRole, 0, 0}},
	ast.Database:             &databaseFunctionClass{baseFunctionClass{ast.Database, 0, 0}},
	ast.CurrentResourceGroup: &currentResourceGroupFunctionClass{baseFunctionClass{ast.CurrentResourceGroup, 0, 0}},

	// This function is a synonym for DATABASE().
	// See http://dev.mysql.com/doc/refman/5.7/en/information-functions.html#function_schema
	ast.Schema:       &databaseFunctionClass{baseFunctionClass{ast.Schema, 0, 0}},
	ast.FoundRows:    &foundRowsFunctionClass{baseFunctionClass{ast.FoundRows, 0, 0}},
	ast.LastInsertId: &lastInsertIDFunctionClass{baseFunctionClass{ast.LastInsertId, 0, 1}},
	ast.User:         &userFunctionClass{baseFunctionClass{ast.User, 0, 0}},
	ast.Version:      &versionFunctionClass{baseFunctionClass{ast.Version, 0, 0}},
	ast.Benchmark:    &benchmarkFunctionClass{baseFunctionClass{ast.Benchmark, 2, 2}},
	ast.Charset:      &charsetFunctionClass{baseFunctionClass{ast.Charset, 1, 1}},
	ast.Coercibility: &coercibilityFunctionClass{baseFunctionClass{ast.Coercibility, 1, 1}},
	ast.Collation:    &collationFunctionClass{baseFunctionClass{ast.Collation, 1, 1}},
	ast.RowCount:     &rowCountFunctionClass{baseFunctionClass{ast.RowCount, 0, 0}},
	ast.SessionUser:  &userFunctionClass{baseFunctionClass{ast.SessionUser, 0, 0}},
	ast.SystemUser:   &userFunctionClass{baseFunctionClass{ast.SystemUser, 0, 0}},

	// See https://dev.mysql.com/doc/refman/8.0/en/performance-schema-functions.html
	ast.FormatBytes:    &formatBytesFunctionClass{baseFunctionClass{ast.FormatBytes, 1, 1}},
	ast.FormatNanoTime: &formatNanoTimeFunctionClass{baseFunctionClass{ast.FormatNanoTime, 1, 1}},

	// control functions
	ast.If:     &ifFunctionClass{baseFunctionClass{ast.If, 3, 3}},
	ast.Ifnull: &ifNullFunctionClass{baseFunctionClass{ast.Ifnull, 2, 2}},

	// miscellaneous functions
	ast.Sleep:           &sleepFunctionClass{baseFunctionClass{ast.Sleep, 1, 1}},
	ast.AnyValue:        &anyValueFunctionClass{baseFunctionClass{ast.AnyValue, 1, 1}},
	ast.DefaultFunc:     &defaultFunctionClass{baseFunctionClass{ast.DefaultFunc, 1, 1}},
	ast.InetAton:        &inetAtonFunctionClass{baseFunctionClass{ast.InetAton, 1, 1}},
	ast.InetNtoa:        &inetNtoaFunctionClass{baseFunctionClass{ast.InetNtoa, 1, 1}},
	ast.Inet6Aton:       &inet6AtonFunctionClass{baseFunctionClass{ast.Inet6Aton, 1, 1}},
	ast.Inet6Ntoa:       &inet6NtoaFunctionClass{baseFunctionClass{ast.Inet6Ntoa, 1, 1}},
	ast.IsFreeLock:      &isFreeLockFunctionClass{baseFunctionClass{ast.IsFreeLock, 1, 1}},
	ast.IsIPv4:          &isIPv4FunctionClass{baseFunctionClass{ast.IsIPv4, 1, 1}},
	ast.IsIPv4Compat:    &isIPv4CompatFunctionClass{baseFunctionClass{ast.IsIPv4Compat, 1, 1}},
	ast.IsIPv4Mapped:    &isIPv4MappedFunctionClass{baseFunctionClass{ast.IsIPv4Mapped, 1, 1}},
	ast.IsIPv6:          &isIPv6FunctionClass{baseFunctionClass{ast.IsIPv6, 1, 1}},
	ast.IsUsedLock:      &isUsedLockFunctionClass{baseFunctionClass{ast.IsUsedLock, 1, 1}},
	ast.IsUUID:          &isUUIDFunctionClass{baseFunctionClass{ast.IsUUID, 1, 1}},
	ast.NameConst:       &nameConstFunctionClass{baseFunctionClass{ast.NameConst, 2, 2}},
	ast.ReleaseAllLocks: &releaseAllLocksFunctionClass{baseFunctionClass{ast.ReleaseAllLocks, 0, 0}},
	ast.UUID:            &uuidFunctionClass{baseFunctionClass{ast.UUID, 0, 0}},
	ast.UUIDShort:       &uuidShortFunctionClass{baseFunctionClass{ast.UUIDShort, 0, 0}},
	ast.VitessHash:      &vitessHashFunctionClass{baseFunctionClass{ast.VitessHash, 1, 1}},
	ast.UUIDToBin:       &uuidToBinFunctionClass{baseFunctionClass{ast.UUIDToBin, 1, 2}},
	ast.BinToUUID:       &binToUUIDFunctionClass{baseFunctionClass{ast.BinToUUID, 1, 2}},
	ast.TiDBShard:       &tidbShardFunctionClass{baseFunctionClass{ast.TiDBShard, 1, 1}},
	ast.TiDBRowChecksum: &tidbRowChecksumFunctionClass{baseFunctionClass{ast.TiDBRowChecksum, 0, 0}},
	ast.Grouping:        &groupingImplFunctionClass{baseFunctionClass{ast.Grouping, 1, 1}},

	ast.GetLock:     &lockFunctionClass{baseFunctionClass{ast.GetLock, 2, 2}},
	ast.ReleaseLock: &releaseLockFunctionClass{baseFunctionClass{ast.ReleaseLock, 1, 1}},

	ast.LogicAnd:           &logicAndFunctionClass{baseFunctionClass{ast.LogicAnd, 2, 2}},
	ast.LogicOr:            &logicOrFunctionClass{baseFunctionClass{ast.LogicOr, 2, 2}},
	ast.LogicXor:           &logicXorFunctionClass{baseFunctionClass{ast.LogicXor, 2, 2}},
	ast.GE:                 &compareFunctionClass{baseFunctionClass{ast.GE, 2, 2}, opcode.GE},
	ast.LE:                 &compareFunctionClass{baseFunctionClass{ast.LE, 2, 2}, opcode.LE},
	ast.EQ:                 &compareFunctionClass{baseFunctionClass{ast.EQ, 2, 2}, opcode.EQ},
	ast.NE:                 &compareFunctionClass{baseFunctionClass{ast.NE, 2, 2}, opcode.NE},
	ast.LT:                 &compareFunctionClass{baseFunctionClass{ast.LT, 2, 2}, opcode.LT},
	ast.GT:                 &compareFunctionClass{baseFunctionClass{ast.GT, 2, 2}, opcode.GT},
	ast.NullEQ:             &compareFunctionClass{baseFunctionClass{ast.NullEQ, 2, 2}, opcode.NullEQ},
	ast.Plus:               &arithmeticPlusFunctionClass{baseFunctionClass{ast.Plus, 2, 2}},
	ast.Minus:              &arithmeticMinusFunctionClass{baseFunctionClass{ast.Minus, 2, 2}},
	ast.Mod:                &arithmeticModFunctionClass{baseFunctionClass{ast.Mod, 2, 2}},
	ast.Div:                &arithmeticDivideFunctionClass{baseFunctionClass{ast.Div, 2, 2}},
	ast.Mul:                &arithmeticMultiplyFunctionClass{baseFunctionClass{ast.Mul, 2, 2}},
	ast.IntDiv:             &arithmeticIntDivideFunctionClass{baseFunctionClass{ast.IntDiv, 2, 2}},
	ast.BitNeg:             &bitNegFunctionClass{baseFunctionClass{ast.BitNeg, 1, 1}},
	ast.And:                &bitAndFunctionClass{baseFunctionClass{ast.And, 2, 2}},
	ast.LeftShift:          &leftShiftFunctionClass{baseFunctionClass{ast.LeftShift, 2, 2}},
	ast.RightShift:         &rightShiftFunctionClass{baseFunctionClass{ast.RightShift, 2, 2}},
	ast.UnaryNot:           &unaryNotFunctionClass{baseFunctionClass{ast.UnaryNot, 1, 1}},
	ast.Or:                 &bitOrFunctionClass{baseFunctionClass{ast.Or, 2, 2}},
	ast.Xor:                &bitXorFunctionClass{baseFunctionClass{ast.Xor, 2, 2}},
	ast.UnaryMinus:         &unaryMinusFunctionClass{baseFunctionClass{ast.UnaryMinus, 1, 1}},
	ast.In:                 &inFunctionClass{baseFunctionClass{ast.In, 2, -1}},
	ast.IsTruthWithoutNull: &isTrueOrFalseFunctionClass{baseFunctionClass{ast.IsTruthWithoutNull, 1, 1}, opcode.IsTruth, false},
	ast.IsTruthWithNull:    &isTrueOrFalseFunctionClass{baseFunctionClass{ast.IsTruthWithNull, 1, 1}, opcode.IsTruth, true},
	ast.IsFalsity:          &isTrueOrFalseFunctionClass{baseFunctionClass{ast.IsFalsity, 1, 1}, opcode.IsFalsity, false},
	ast.Like:               &likeFunctionClass{baseFunctionClass{ast.Like, 3, 3}},
	ast.Ilike:              &ilikeFunctionClass{baseFunctionClass{ast.Ilike, 3, 3}},
	ast.Regexp:             &regexpLikeFunctionClass{baseFunctionClass{ast.Regexp, 2, 2}},
	ast.RegexpLike:         &regexpLikeFunctionClass{baseFunctionClass{ast.RegexpLike, 2, 3}},
	ast.RegexpSubstr:       &regexpSubstrFunctionClass{baseFunctionClass{ast.RegexpSubstr, 2, 5}},
	ast.RegexpInStr:        &regexpInStrFunctionClass{baseFunctionClass{ast.RegexpInStr, 2, 6}},
	ast.RegexpReplace:      &regexpReplaceFunctionClass{baseFunctionClass{ast.RegexpReplace, 3, 6}},
	ast.Case:               &caseWhenFunctionClass{baseFunctionClass{ast.Case, 1, -1}},
	ast.RowFunc:            &rowFunctionClass{baseFunctionClass{ast.RowFunc, 2, -1}},
	ast.SetVar:             &setVarFunctionClass{baseFunctionClass{ast.SetVar, 2, 2}},
	ast.BitCount:           &bitCountFunctionClass{baseFunctionClass{ast.BitCount, 1, 1}},
	ast.GetParam:           &getParamFunctionClass{baseFunctionClass{ast.GetParam, 1, 1}},

	// encryption and compression functions
	ast.AesDecrypt:               &aesDecryptFunctionClass{baseFunctionClass{ast.AesDecrypt, 2, 3}},
	ast.AesEncrypt:               &aesEncryptFunctionClass{baseFunctionClass{ast.AesEncrypt, 2, 3}},
	ast.Compress:                 &compressFunctionClass{baseFunctionClass{ast.Compress, 1, 1}},
	ast.Decode:                   &decodeFunctionClass{baseFunctionClass{ast.Decode, 2, 2}},
	ast.Encode:                   &encodeFunctionClass{baseFunctionClass{ast.Encode, 2, 2}},
	ast.MD5:                      &md5FunctionClass{baseFunctionClass{ast.MD5, 1, 1}},
	ast.PasswordFunc:             &passwordFunctionClass{baseFunctionClass{ast.PasswordFunc, 1, 1}},
	ast.RandomBytes:              &randomBytesFunctionClass{baseFunctionClass{ast.RandomBytes, 1, 1}},
	ast.SHA1:                     &sha1FunctionClass{baseFunctionClass{ast.SHA1, 1, 1}},
	ast.SHA:                      &sha1FunctionClass{baseFunctionClass{ast.SHA, 1, 1}},
	ast.SHA2:                     &sha2FunctionClass{baseFunctionClass{ast.SHA2, 2, 2}},
	ast.SM3:                      &sm3FunctionClass{baseFunctionClass{ast.SM3, 1, 1}},
	ast.Uncompress:               &uncompressFunctionClass{baseFunctionClass{ast.Uncompress, 1, 1}},
	ast.UncompressedLength:       &uncompressedLengthFunctionClass{baseFunctionClass{ast.UncompressedLength, 1, 1}},
	ast.ValidatePasswordStrength: &validatePasswordStrengthFunctionClass{baseFunctionClass{ast.ValidatePasswordStrength, 1, 1}},

	// json functions
	ast.JSONType:          &jsonTypeFunctionClass{baseFunctionClass{ast.JSONType, 1, 1}},
	ast.JSONExtract:       &jsonExtractFunctionClass{baseFunctionClass{ast.JSONExtract, 2, -1}},
	ast.JSONUnquote:       &jsonUnquoteFunctionClass{baseFunctionClass{ast.JSONUnquote, 1, 1}},
	ast.JSONSet:           &jsonSetFunctionClass{baseFunctionClass{ast.JSONSet, 3, -1}},
	ast.JSONInsert:        &jsonInsertFunctionClass{baseFunctionClass{ast.JSONInsert, 3, -1}},
	ast.JSONReplace:       &jsonReplaceFunctionClass{baseFunctionClass{ast.JSONReplace, 3, -1}},
	ast.JSONRemove:        &jsonRemoveFunctionClass{baseFunctionClass{ast.JSONRemove, 2, -1}},
	ast.JSONMerge:         &jsonMergeFunctionClass{baseFunctionClass{ast.JSONMerge, 2, -1}},
	ast.JSONObject:        &jsonObjectFunctionClass{baseFunctionClass{ast.JSONObject, 0, -1}},
	ast.JSONArray:         &jsonArrayFunctionClass{baseFunctionClass{ast.JSONArray, 0, -1}},
	ast.JSONMemberOf:      &jsonMemberOfFunctionClass{baseFunctionClass{ast.JSONMemberOf, 2, 2}},
	ast.JSONContains:      &jsonContainsFunctionClass{baseFunctionClass{ast.JSONContains, 2, 3}},
	ast.JSONOverlaps:      &jsonOverlapsFunctionClass{baseFunctionClass{ast.JSONOverlaps, 2, 2}},
	ast.JSONContainsPath:  &jsonContainsPathFunctionClass{baseFunctionClass{ast.JSONContainsPath, 3, -1}},
	ast.JSONValid:         &jsonValidFunctionClass{baseFunctionClass{ast.JSONValid, 1, 1}},
	ast.JSONArrayAppend:   &jsonArrayAppendFunctionClass{baseFunctionClass{ast.JSONArrayAppend, 3, -1}},
	ast.JSONArrayInsert:   &jsonArrayInsertFunctionClass{baseFunctionClass{ast.JSONArrayInsert, 3, -1}},
	ast.JSONMergePatch:    &jsonMergePatchFunctionClass{baseFunctionClass{ast.JSONMergePatch, 2, -1}},
	ast.JSONMergePreserve: &jsonMergePreserveFunctionClass{baseFunctionClass{ast.JSONMergePreserve, 2, -1}},
	ast.JSONPretty:        &jsonPrettyFunctionClass{baseFunctionClass{ast.JSONPretty, 1, 1}},
	ast.JSONQuote:         &jsonQuoteFunctionClass{baseFunctionClass{ast.JSONQuote, 1, 1}},
	ast.JSONSchemaValid:   &jsonSchemaValidFunctionClass{baseFunctionClass{ast.JSONSchemaValid, 2, 2}},
	ast.JSONSearch:        &jsonSearchFunctionClass{baseFunctionClass{ast.JSONSearch, 3, -1}},
	ast.JSONStorageFree:   &jsonStorageFreeFunctionClass{baseFunctionClass{ast.JSONStorageFree, 1, 1}},
	ast.JSONStorageSize:   &jsonStorageSizeFunctionClass{baseFunctionClass{ast.JSONStorageSize, 1, 1}},
	ast.JSONDepth:         &jsonDepthFunctionClass{baseFunctionClass{ast.JSONDepth, 1, 1}},
	ast.JSONKeys:          &jsonKeysFunctionClass{baseFunctionClass{ast.JSONKeys, 1, 2}},
	ast.JSONLength:        &jsonLengthFunctionClass{baseFunctionClass{ast.JSONLength, 1, 2}},

	// vector functions (TiDB extension)
	ast.VecDims:                 &vecDimsFunctionClass{baseFunctionClass{ast.VecDims, 1, 1}},
	ast.VecL1Distance:           &vecL1DistanceFunctionClass{baseFunctionClass{ast.VecL1Distance, 2, 2}},
	ast.VecL2Distance:           &vecL2DistanceFunctionClass{baseFunctionClass{ast.VecL2Distance, 2, 2}},
	ast.VecNegativeInnerProduct: &vecNegativeInnerProductFunctionClass{baseFunctionClass{ast.VecNegativeInnerProduct, 2, 2}},
	ast.VecCosineDistance:       &vecCosineDistanceFunctionClass{baseFunctionClass{ast.VecCosineDistance, 2, 2}},
	ast.VecL2Norm:               &vecL2NormFunctionClass{baseFunctionClass{ast.VecL2Norm, 1, 1}},
	ast.VecFromText:             &vecFromTextFunctionClass{baseFunctionClass{ast.VecFromText, 1, 1}},
	ast.VecAsText:               &vecAsTextFunctionClass{baseFunctionClass{ast.VecAsText, 1, 1}},

	// fts functions
	ast.FTSMatchWord: &ftsMatchWordFunctionClass{baseFunctionClass{ast.FTSMatchWord, 2, 2}},

	// TiDB internal function.
	ast.TiDBDecodeKey:       &tidbDecodeKeyFunctionClass{baseFunctionClass{ast.TiDBDecodeKey, 1, 1}},
	ast.TiDBMVCCInfo:        &tidbMVCCInfoFunctionClass{baseFunctionClass: baseFunctionClass{ast.TiDBMVCCInfo, 1, 1}},
	ast.TiDBEncodeRecordKey: &tidbEncodeRecordKeyClass{baseFunctionClass{ast.TiDBEncodeRecordKey, 3, -1}},
	ast.TiDBEncodeIndexKey:  &tidbEncodeIndexKeyClass{baseFunctionClass{ast.TiDBEncodeIndexKey, 4, -1}},
	// This function is used to show tidb-server version info.
	ast.TiDBVersion:          &tidbVersionFunctionClass{baseFunctionClass{ast.TiDBVersion, 0, 0}},
	ast.TiDBIsDDLOwner:       &tidbIsDDLOwnerFunctionClass{baseFunctionClass{ast.TiDBIsDDLOwner, 0, 0}},
	ast.TiDBDecodePlan:       &tidbDecodePlanFunctionClass{baseFunctionClass{ast.TiDBDecodePlan, 1, 1}},
	ast.TiDBDecodeBinaryPlan: &tidbDecodePlanFunctionClass{baseFunctionClass{ast.TiDBDecodeBinaryPlan, 1, 1}},
	ast.TiDBDecodeSQLDigests: &tidbDecodeSQLDigestsFunctionClass{baseFunctionClass: baseFunctionClass{ast.TiDBDecodeSQLDigests, 1, 2}},
	ast.TiDBEncodeSQLDigest:  &tidbEncodeSQLDigestFunctionClass{baseFunctionClass{ast.TiDBEncodeSQLDigest, 1, 1}},

	// TiDB Sequence function.
	ast.NextVal: &nextValFunctionClass{baseFunctionClass{ast.NextVal, 1, 1}},
	ast.LastVal: &lastValFunctionClass{baseFunctionClass{ast.LastVal, 1, 1}},
	ast.SetVal:  &setValFunctionClass{baseFunctionClass{ast.SetVal, 2, 2}},
}

// IsFunctionSupported check if given function name is a builtin sql function.
func IsFunctionSupported(name string) bool {
	_, ok := funcs[name]
	return ok
}

// GetDisplayName translate a function name to its display name
func GetDisplayName(name string) string {
	if funClass, ok := funcs[name]; ok {
		if funClass, ok := funClass.(functionClassWithName); ok {
			return funClass.getDisplayName()
		}
	}

	return name
}

// GetBuiltinList returns a list of builtin functions
func GetBuiltinList() []string {
	res := make([]string, 0, len(funcs))
	notImplementedFunctions := []string{ast.RowFunc, ast.IsTruthWithNull}
	for funcName := range funcs {
		skipFunc := false
		// Skip not implemented functions
		for _, notImplFunc := range notImplementedFunctions {
			if funcName == notImplFunc {
				skipFunc = true
			}
		}
		// Skip literal functions
		// (their names are not readable: 'tidb`.(dateliteral, for example)
		// See: https://github.com/pingcap/parser/pull/591
		if strings.HasPrefix(funcName, "'tidb`.(") {
			skipFunc = true
		}
		if skipFunc {
			continue
		}
		res = append(res, funcName)
	}

	extensionFuncs.Range(func(key, _ any) bool {
		funcName := key.(string)
		res = append(res, funcName)
		return true
	})

	slices.Sort(res)
	return res
}

func (b *baseBuiltinFunc) setDecimalAndFlenForDatetime(fsp int) {
	b.tp.SetDecimalUnderLimit(fsp)
	b.tp.SetFlenUnderLimit(mysql.MaxDatetimeWidthNoFsp + fsp)
	if fsp > 0 {
		// Add the length for `.`.
		b.tp.SetFlenUnderLimit(b.tp.GetFlen() + 1)
	}
}

func (b *baseBuiltinFunc) setDecimalAndFlenForDate() {
	b.tp.SetDecimal(0)
	b.tp.SetFlen(mysql.MaxDateWidth)
	b.tp.SetType(mysql.TypeDate)
}

func (b *baseBuiltinFunc) setDecimalAndFlenForTime(fsp int) {
	b.tp.SetDecimalUnderLimit(fsp)
	b.tp.SetFlenUnderLimit(mysql.MaxDurationWidthNoFsp + fsp)
	if fsp > 0 {
		// Add the length for `.`.
		b.tp.SetFlenUnderLimit(b.tp.GetFlen() + 1)
	}
}

const emptyBaseBuiltinFunc = int64(unsafe.Sizeof(baseBuiltinFunc{}))
const onceSize = int64(unsafe.Sizeof(sync.Once{}))

// MemoryUsage return the memory usage of baseBuiltinFunc
func (b *baseBuiltinFunc) MemoryUsage() (sum int64) {
	if b == nil {
		return
	}

	sum = emptyBaseBuiltinFunc + int64(len(b.charset)+len(b.collation))
	if b.bufAllocator != nil {
		sum += b.bufAllocator.MemoryUsage()
	}
	if b.tp != nil {
		sum += b.tp.MemoryUsage()
	}
	if b.childrenVectorizedOnce != nil {
		sum += onceSize
	}
	for _, e := range b.args {
		sum += e.MemoryUsage()
	}
	return
}

type builtinFuncCacheItem[T any] struct {
	ctxID uint64
	item  T
}

type builtinFuncCache[T any] struct {
	sync.Mutex
	cached atomic.Pointer[builtinFuncCacheItem[T]]
}

func (c *builtinFuncCache[T]) getCache(ctxID uint64) (v T, ok bool) {
	if p := c.cached.Load(); p != nil && p.ctxID == ctxID {
		return p.item, true
	}
	return v, false
}

func (c *builtinFuncCache[T]) getOrInitCache(ctx EvalContext, constructCache func() (T, error)) (T, error) {
	intest.Assert(constructCache != nil)
	ctxID := ctx.CtxID()
	if item, ok := c.getCache(ctxID); ok {
		return item, nil
	}

	c.Lock()
	defer c.Unlock()
	if item, ok := c.getCache(ctxID); ok {
		return item, nil
	}

	item, err := constructCache()
	if err != nil {
		var def T
		return def, err
	}

	c.cached.Store(&builtinFuncCacheItem[T]{
		ctxID: ctxID,
		item:  item,
	})
	return item, nil
}
