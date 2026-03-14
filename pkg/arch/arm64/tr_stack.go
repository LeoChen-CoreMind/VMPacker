package arm64

import (
	"encoding/binary"
	"fmt"

	"github.com/vmpacker/pkg/vm"
)

// ============================================================
// 栈模式翻译器 — 所有 ARM64 指令转为纯栈操作
//
// 翻译策略:
//   register-based: emit(OpAdd, rd, rn, rm)
//   stack-based:    VLOAD(rn) → VLOAD(rm) → S_ADD → VSTORE(rd)
//
// 优势: 彻底消除寄存器冲突，无需 pickTemp/pickTemp2
// ============================================================

// ---- 栈模式 emit 辅助函数 ----

// sVload push R[reg] onto eval stack
func (t *Translator) sVload(reg byte) {
	t.emit(vm.OpSVload, reg)
}

// sVstore pop eval stack → R[reg]
func (t *Translator) sVstore(reg byte) {
	t.emit(vm.OpSVstore, reg)
}

// sPushImm32 push a 32-bit immediate
func (t *Translator) sPushImm32(v uint32) {
	t.emit(vm.OpSPushImm32)
	t.emitU32(v)
}

// sPushImm64 push a 64-bit immediate
func (t *Translator) sPushImm64(v uint64) {
	t.emit(vm.OpSPushImm64)
	t.emitU64(v)
}

// sPushImm push immediate, auto-select 32 vs 64 bit
func (t *Translator) sPushImm(v uint64) {
	if v <= 0xFFFFFFFF {
		t.sPushImm32(uint32(v))
	} else {
		t.sPushImm64(v)
	}
}

// sDup duplicate TOS
func (t *Translator) sDup() { t.emit(vm.OpSDup) }

// sSwap swap TOS and TOS-1
func (t *Translator) sSwap() { t.emit(vm.OpSSwap) }

// sDrop pop and discard TOS
func (t *Translator) sDrop() { t.emit(vm.OpSDrop) }

// ---- 栈模式翻译函数 ----

// trStackAluReg 翻译三寄存器 ALU (栈模式)
// ARM64: op Xd, Xn, Xm  →  VLOAD(rn) VLOAD(rm) S_OP VSTORE(rd)
func (t *Translator) trStackAluReg(inst vm.Instruction, sOp byte) error {
	rd, rn, rm, err := t.mapReg3(inst)
	if err != nil {
		return err
	}

	// XZR 处理: push 0 而不是 VLOAD
	t.pushRegOrZero(inst.Rn, rn)

	// 移位处理
	if inst.Shift != 0 {
		t.pushRegOrZero(inst.Rm, rm)
		t.emitShiftOnStack(inst.ShiftType, uint32(inst.Shift), inst.SF)
	} else {
		t.pushRegOrZero(inst.Rm, rm)
	}

	if sOp == vm.OpSRor {
		if inst.SF {
			t.sPushImm32(64)
		} else {
			t.sPushImm32(32)
		}
	}

	t.emit(sOp) // 二元操作

	if !inst.SF {
		t.emit(vm.OpSTrunc32) // W 寄存器模式截断
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop() // 结果丢弃
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackAluRegFlags 翻译三寄存器 ALU + 设置标志位 (栈模式)
func (t *Translator) trStackAluRegFlags(inst vm.Instruction, sOp byte, setFlags bool) error {
	rd, rn, rm, err := t.mapReg3(inst)
	if err != nil {
		return err
	}

	t.pushRegOrZero(inst.Rn, rn)

	if inst.Shift != 0 {
		t.pushRegOrZero(inst.Rm, rm)
		t.emitShiftOnStack(inst.ShiftType, uint32(inst.Shift), inst.SF)
	} else {
		t.pushRegOrZero(inst.Rm, rm)
	}

	t.emit(sOp)

	if setFlags {
		t.sDup()          // duplicate result for CMP
		t.sPushImm32(0)   // push 0
		t.emit(vm.OpSCmp) // compare result with 0 → set flags
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackAluImm 翻译寄存器+立即数 ALU (栈模式)
// ARM64: op Xd, Xn, #imm  →  VLOAD(rn) PUSH(imm) S_OP VSTORE(rd)
func (t *Translator) trStackAluImm(inst vm.Instruction, sOp byte) error {
	return t.trStackAluImmFlags(inst, sOp, false)
}

// trStackAluImmFlags 翻译寄存器+立即数 ALU + 标志位 (栈模式)
func (t *Translator) trStackAluImmFlags(inst vm.Instruction, sOp byte, setFlags bool) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}

	t.pushRegOrZero(inst.Rn, rn)
	t.sPushImm(uint64(inst.Imm))

	t.emit(sOp)

	if setFlags {
		t.sDup()
		t.sPushImm32(0)
		t.emit(vm.OpSCmp)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackUnary 翻译一元操作 (栈模式)
// ARM64: op Xd, Xn  →  VLOAD(rn) S_OP VSTORE(rd)
func (t *Translator) trStackUnary(inst vm.Instruction, sOp byte) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}

	t.sVload(rn)
	t.emit(sOp)

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	t.sVstore(rd)
	return nil
}

// trStackMov 翻译 MOV (栈模式)
// MOVZ: Xd = imm << shift
// MOVN: Xd = ~(imm << shift)
func (t *Translator) trStackMov(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	imm := uint64(inst.Imm) << uint64(inst.Shift)
	t.sPushImm(imm)

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	t.sVstore(rd)
	return nil
}

// trStackMovN 翻译 MOVN (栈模式)
func (t *Translator) trStackMovN(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	val := uint64(inst.Imm) << uint64(inst.Shift)
	val = ^val
	if !inst.SF {
		val &= 0xFFFFFFFF
	}
	t.sPushImm(val)
	t.sVstore(rd)
	return nil
}

// trStackMovK 翻译 MOVK (栈模式)
// 保留 Rd 其他字段，仅替换指定 16-bit 段
func (t *Translator) trStackMovK(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	hw := uint64(inst.Shift) // 0, 16, 32, 48
	imm := uint64(inst.Imm)
	mask := uint64(0xFFFF) << hw // 要清除的 16-bit 区域

	// Rd = (Rd & ~mask) | (imm << hw)
	t.sVload(rd)          // push Rd
	t.sPushImm(^mask)     // push ~mask
	t.emit(vm.OpSAnd)     // Rd & ~mask
	t.sPushImm(imm << hw) // push (imm << hw)
	t.emit(vm.OpSOr)      // (Rd & ~mask) | (imm << hw)

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	t.sVstore(rd)
	return nil
}

// trStackCmp 翻译 CMP reg,reg (栈模式)
// CMP Xn, Xm → VLOAD(rn) VLOAD(rm) S_CMP
func (t *Translator) trStackCmp(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	t.pushRegOrZero(inst.Rn, rn)
	t.pushRegOrZero(inst.Rm, rm)
	t.emit(vm.OpSCmp)
	return nil
}

// trStackCmpImm 翻译 CMP reg,#imm (栈模式)
func (t *Translator) trStackCmpImm(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}

	t.pushRegOrZero(inst.Rn, rn)
	t.sPushImm(uint64(inst.Imm))
	t.emit(vm.OpSCmp)
	return nil
}

// trStackLoad 翻译 LDR (栈模式)
// LDR Rd, [Rn, #off] → VLOAD(rn) PUSH(off) S_ADD S_LD{8|16|32|64} VSTORE(rd)
func (t *Translator) trStackLoad(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}

	op := Op(inst.Op)
	var sLdOp byte
	switch op {
	case LDRB_IMM:
		sLdOp = vm.OpSLd8
	case LDRH_IMM:
		sLdOp = vm.OpSLd16
	case LDRSB_IMM:
		sLdOp = vm.OpSLd8
	case LDRSH_IMM:
		sLdOp = vm.OpSLd16
	case LDRSW_IMM:
		sLdOp = vm.OpSLd32
	case LDR_IMM:
		if inst.SF {
			sLdOp = vm.OpSLd64
		} else {
			sLdOp = vm.OpSLd32
		}
	default:
		sLdOp = vm.OpSLd64
	}

	// 回写辅助函数 (pre/post index)
	emitWriteback := func() {
		t.sVload(rn)
		wbImm := inst.Imm
		if wbImm >= 0 {
			t.sPushImm(uint64(wbImm))
			t.emit(vm.OpSAdd)
		} else {
			t.sPushImm(uint64(-wbImm))
			t.emit(vm.OpSSub)
		}
		t.sVstore(rn) // Rn updated
	}

	if inst.WB == 3 {
		// pre-index: Rn += imm first, then load [Rn]
		emitWriteback()
		t.sVload(rn)
		t.emit(sLdOp)
	} else if inst.WB == 1 {
		// post-index: load [Rn], then Rn += imm
		t.sVload(rn)
		t.emit(sLdOp)
		if inst.Rd != vm.REG_XZR {
			t.sVstore(rd)
		} else {
			t.sDrop()
		}
		emitWriteback()
		goto signext
	} else {
		// offset mode
		t.sVload(rn)
		if inst.Imm != 0 {
			if inst.Imm > 0 {
				t.sPushImm(uint64(inst.Imm))
				t.emit(vm.OpSAdd)
			} else {
				t.sPushImm(uint64(-inst.Imm))
				t.emit(vm.OpSSub)
			}
		}
		t.emit(sLdOp)
	}

	// 符号扩展
	if op == LDRSW_IMM {
		t.emit(vm.OpSSext32)
	}
	if op == LDRSB_IMM {
		// sext 8→64: push 56, S_SHL, push 56, S_ASR
		t.sPushImm32(56)
		t.emit(vm.OpSShl)
		t.sPushImm32(56)
		t.emit(vm.OpSAsr)
	}
	if op == LDRSH_IMM {
		// sext 16→64: push 48, S_SHL, push 48, S_ASR
		t.sPushImm32(48)
		t.emit(vm.OpSShl)
		t.sPushImm32(48)
		t.emit(vm.OpSAsr)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil

signext:
	// post-index path: rd already stored, handle sign extension
	if op == LDRSW_IMM || op == LDRSB_IMM || op == LDRSH_IMM {
		t.sVload(rd)
		if op == LDRSW_IMM {
			t.emit(vm.OpSSext32)
		}
		if op == LDRSB_IMM {
			t.sPushImm32(56)
			t.emit(vm.OpSShl)
			t.sPushImm32(56)
			t.emit(vm.OpSAsr)
		}
		if op == LDRSH_IMM {
			t.sPushImm32(48)
			t.emit(vm.OpSShl)
			t.sPushImm32(48)
			t.emit(vm.OpSAsr)
		}
		t.sVstore(rd)
	}
	return nil
}

// trStackStore 翻译 STR (栈模式)
// STR Rt, [Rn, #off] → VLOAD(rn) PUSH(off) S_ADD VLOAD(rt) S_ST{8|16|32|64}
func (t *Translator) trStackStore(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rd, err := t.mapReg(inst.Rd) // Rt = source value
	if err != nil {
		return err
	}

	op := Op(inst.Op)
	var sStOp byte
	switch op {
	case STRB_IMM:
		sStOp = vm.OpSSt8
	case STRH_IMM:
		sStOp = vm.OpSSt16
	case STR_IMM:
		if inst.SF {
			sStOp = vm.OpSSt64
		} else {
			sStOp = vm.OpSSt32
		}
	default:
		sStOp = vm.OpSSt64
	}

	emitWriteback := func() {
		t.sVload(rn)
		wbImm := inst.Imm
		if wbImm >= 0 {
			t.sPushImm(uint64(wbImm))
			t.emit(vm.OpSAdd)
		} else {
			t.sPushImm(uint64(-wbImm))
			t.emit(vm.OpSSub)
		}
		t.sVstore(rn)
	}

	if inst.WB == 3 {
		// pre-index: Rn += imm, then store [Rn]
		emitWriteback()
		// addr
		t.sVload(rn)
		// value
		t.pushRegOrZero(inst.Rd, rd)
		t.emit(sStOp)
	} else if inst.WB == 1 {
		// post-index: store [Rn], then Rn += imm
		t.sVload(rn)
		t.pushRegOrZero(inst.Rd, rd)
		t.emit(sStOp)
		emitWriteback()
	} else {
		// offset mode
		t.sVload(rn)
		if inst.Imm != 0 {
			if inst.Imm > 0 {
				t.sPushImm(uint64(inst.Imm))
				t.emit(vm.OpSAdd)
			} else {
				t.sPushImm(uint64(-inst.Imm))
				t.emit(vm.OpSSub)
			}
		}
		t.pushRegOrZero(inst.Rd, rd)
		t.emit(sStOp)
	}

	return nil
}

// trStackMovReg 翻译 MOV Xd, Xn (栈模式)
func (t *Translator) trStackMovReg(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	if inst.Rn == vm.REG_XZR {
		t.sPushImm32(0)
	} else {
		rn, err := t.mapReg(inst.Rn)
		if err != nil {
			return err
		}
		t.sVload(rn)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackCBZ 翻译 CBZ/CBNZ (栈模式)
func (t *Translator) trStackCBZ(inst vm.Instruction, isZero bool) error {
	target := inst.Offset + int(inst.Imm)

	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	// 纯栈比较: VLOAD(rd) PUSH(0) S_CMP
	t.sVload(rd)
	t.sPushImm32(0)
	t.emit(vm.OpSCmp)

	var vmOp byte
	if isZero {
		vmOp = vm.OpJe
	} else {
		vmOp = vm.OpJne
	}

	t.emit(vmOp)
	fixPos := t.pos()
	t.emitU32(0)
	t.fixups = append(t.fixups, branchFixup{vmOffset: fixPos, arm64Target: target})
	return nil
}

// trStackMADD 翻译 MADD/MSUB (栈模式)
// MADD: Rd = Ra + Rn * Rm
// MSUB: Rd = Ra - Rn * Rm
func (t *Translator) trStackMADD(inst vm.Instruction, isSub bool) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	// Ra from bits[14:10]
	ra := byte((inst.Raw >> 10) & 0x1F)

	// push Ra
	if ra == 31 {
		t.sPushImm32(0) // XZR
	} else {
		t.sVload(ra)
	}

	// push Rn * Rm
	t.pushRegOrZero(inst.Rn, rn)
	t.pushRegOrZero(inst.Rm, rm)
	t.emit(vm.OpSMul)

	if isSub {
		t.emit(vm.OpSSub) // Ra - (Rn*Rm)
	} else {
		t.emit(vm.OpSAdd) // Ra + (Rn*Rm)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackCSEL 翻译 CSEL/CSINC/CSINV/CSNEG (栈模式)
func (t *Translator) trStackCSEL(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	// 条件码映射
	var vmOp byte
	switch inst.Cond {
	case COND_EQ:
		vmOp = vm.OpJe
	case COND_NE:
		vmOp = vm.OpJne
	case COND_LT:
		vmOp = vm.OpJl
	case COND_GE:
		vmOp = vm.OpJge
	case COND_GT:
		vmOp = vm.OpJgt
	case COND_LE:
		vmOp = vm.OpJle
	case COND_CS:
		vmOp = vm.OpJae
	case COND_CC:
		vmOp = vm.OpJb
	case COND_HI:
		vmOp = vm.OpJa
	case COND_LS:
		vmOp = vm.OpJbe
	case COND_MI:
		vmOp = vm.OpJl
	case COND_PL:
		vmOp = vm.OpJge
	default:
		return fmt.Errorf("CSEL: 不支持的条件码 0x%X", inst.Cond)
	}

	// CSEL 的分支逻辑不能用栈操作改写（它用 VM 分支指令）
	// 但 XZR 处理改为栈模式 push 0
	if inst.Rn == vm.REG_XZR {
		t.sPushImm32(0)
		t.sVstore(rn)
	}
	if inst.Rm == vm.REG_XZR {
		t.sPushImm32(0)
		t.sVstore(rm)
	}

	// 条件跳转到 true 路径
	t.emit(vmOp)
	jccPos := t.pos()
	t.emitU32(0)

	// false path: CSEL → Rd=Rm, CSINC → Rd=Rm+1, etc.
	op := Op(inst.Op)
	switch op {
	case CSINC:
		// Rd = Rm + 1
		t.sVload(rm)
		t.sPushImm32(1)
		t.emit(vm.OpSAdd)
		t.sVstore(rd)
	case CSINV:
		// Rd = ~Rm
		t.sVload(rm)
		t.emit(vm.OpSNot)
		t.sVstore(rd)
	case CSNEG:
		// Rd = ~Rm + 1 (= -Rm)
		t.sVload(rm)
		t.emit(vm.OpSNot)
		t.sPushImm32(1)
		t.emit(vm.OpSAdd)
		t.sVstore(rd)
	default:
		// CSEL: Rd = Rm
		t.sVload(rm)
		t.sVstore(rd)
	}

	t.emit(vm.OpJmp)
	jmpPos := t.pos()
	t.emitU32(0)

	// true path: Rd = Rn
	truePos := t.pos()
	t.sVload(rn)
	t.sVstore(rd)
	endPos := t.pos()

	binary.LittleEndian.PutUint32(t.code[jccPos:], uint32(truePos))
	binary.LittleEndian.PutUint32(t.code[jmpPos:], uint32(endPos))

	return nil
}

// ---- 辅助工具函数 ----

// mapReg3 映射 Rd/Rn/Rm 三寄存器 (XZR→16 但不再有冲突顾虑)
func (t *Translator) mapReg3(inst vm.Instruction) (byte, byte, byte, error) {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return 0, 0, 0, err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return 0, 0, 0, err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return 0, 0, 0, err
	}
	return rd, rn, rm, nil
}

// pushRegOrZero push register value, or push 0 if XZR
func (t *Translator) pushRegOrZero(arm64Reg int, vmReg byte) {
	if arm64Reg == vm.REG_XZR {
		t.sPushImm32(0)
	} else {
		t.sVload(vmReg)
	}
}

// emitShiftOnStack 在栈顶值上执行移位操作 (用于 shifted register operands)
// TOS = value to shift, 输出 TOS = shifted value
func (t *Translator) emitShiftOnStack(shiftType int, amount uint32, sf bool) {
	if amount == 0 {
		return
	}

	// 32-bit 模式: 先截断到 32 位
	if !sf {
		t.emit(vm.OpSTrunc32)
	}

	switch shiftType {
	case 0: // LSL
		t.sPushImm32(amount)
		t.emit(vm.OpSShl)
	case 1: // LSR
		t.sPushImm32(amount)
		t.emit(vm.OpSShr)
	case 2: // ASR
		if !sf {
			// 32-bit ASR: 需要先符号扩展
			t.emit(vm.OpSSext32)
			t.sPushImm32(amount)
			t.emit(vm.OpSAsr)
		} else {
			t.sPushImm32(amount)
			t.emit(vm.OpSAsr)
		}
	case 3: // ROR
		if !sf {
			// 32-bit ROR: 用 SHR+SHL+OR 模拟
			shift := amount & 31
			if shift != 0 {
				t.sDup() // dup value
				t.sPushImm32(shift)
				t.emit(vm.OpSShr) // value >> shift
				t.sSwap()         // bring original value up
				t.sPushImm32(32 - shift)
				t.emit(vm.OpSShl) // value << (32-shift)
				t.emit(vm.OpSOr)  // combine
			}
		} else {
			t.sPushImm32(amount)
			t.sPushImm32(64)
			t.emit(vm.OpSRor)
		}
	}

	// 32-bit 模式: 截断移位结果
	if !sf {
		t.emit(vm.OpSTrunc32)
	}
}

// ---- STP/LDP 栈模式翻译 ----

// trStackSTP 翻译 STP (Store Pair) — 栈模式
func (t *Translator) trStackSTP(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rt1, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rt2, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	var sStOp byte
	stride := int64(8)
	if !inst.SF {
		sStOp = vm.OpSSt32
		stride = 4
	} else {
		sStOp = vm.OpSSt64
	}

	emitWriteback := func() {
		t.sVload(rn)
		if inst.Imm >= 0 {
			t.sPushImm(uint64(inst.Imm))
			t.emit(vm.OpSAdd)
		} else {
			t.sPushImm(uint64(-inst.Imm))
			t.emit(vm.OpSSub)
		}
		t.sVstore(rn)
	}

	if inst.WB == 3 {
		// pre-index: Rn += imm, then store [Rn] and [Rn+stride]
		emitWriteback()
		// store Rt1 at [Rn]
		t.sVload(rn)
		t.pushRegOrZero(inst.Rd, rt1)
		t.emit(sStOp)
		// store Rt2 at [Rn+stride]
		t.sVload(rn)
		t.sPushImm(uint64(stride))
		t.emit(vm.OpSAdd)
		t.pushRegOrZero(inst.Rm, rt2)
		t.emit(sStOp)
	} else {
		storeImm := inst.Imm
		if inst.WB == 1 {
			storeImm = 0 // post-index
		}
		// store Rt1 at [Rn+storeImm]
		t.sVload(rn)
		if storeImm != 0 {
			t.sPushImm(uint64(abs64(storeImm)))
			if storeImm > 0 {
				t.emit(vm.OpSAdd)
			} else {
				t.emit(vm.OpSSub)
			}
		}
		t.pushRegOrZero(inst.Rd, rt1)
		t.emit(sStOp)
		// store Rt2 at [Rn+storeImm+stride]
		t.sVload(rn)
		off2 := storeImm + stride
		if off2 != 0 {
			t.sPushImm(uint64(abs64(off2)))
			if off2 > 0 {
				t.emit(vm.OpSAdd)
			} else {
				t.emit(vm.OpSSub)
			}
		}
		t.pushRegOrZero(inst.Rm, rt2)
		t.emit(sStOp)
		// post-index writeback
		if inst.WB == 1 {
			emitWriteback()
		}
	}
	return nil
}

// trStackLDP 翻译 LDP (Load Pair) — 栈模式
func (t *Translator) trStackLDP(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rt1, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rt2, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	var sLdOp byte
	stride := int64(8)
	if !inst.SF {
		sLdOp = vm.OpSLd32
		stride = 4
	} else {
		sLdOp = vm.OpSLd64
	}

	emitWriteback := func() {
		t.sVload(rn)
		if inst.Imm >= 0 {
			t.sPushImm(uint64(inst.Imm))
			t.emit(vm.OpSAdd)
		} else {
			t.sPushImm(uint64(-inst.Imm))
			t.emit(vm.OpSSub)
		}
		t.sVstore(rn)
	}

	if inst.WB == 3 {
		// pre-index
		emitWriteback()
		// Rt1 = [Rn]
		t.sVload(rn)
		t.emit(sLdOp)
		t.sVstore(rt1)
		// Rt2 = [Rn+stride]
		t.sVload(rn)
		t.sPushImm(uint64(stride))
		t.emit(vm.OpSAdd)
		t.emit(sLdOp)
		t.sVstore(rt2)
	} else {
		loadImm := inst.Imm
		if inst.WB == 1 {
			loadImm = 0
		}
		// 栈模式不需要 pickTemp! 当 rt1==rn 时:
		// 先计算 addr2 并保存到栈, 再 load
		// 但更简单的方式: 先计算 base+offset, load rt1, 再计算 base+offset+stride, load rt2

		// 先保存 base 地址到栈: addr_base = Rn + loadImm
		t.sVload(rn)
		if loadImm != 0 {
			t.sPushImm(uint64(abs64(loadImm)))
			if loadImm > 0 {
				t.emit(vm.OpSAdd)
			} else {
				t.emit(vm.OpSSub)
			}
		}
		t.sDup() // duplicate base addr for second load

		// load Rt1 from addr_base
		t.emit(sLdOp)
		t.sVstore(rt1)

		// stack now has: [addr_base]
		// load Rt2 from addr_base + stride
		t.sPushImm(uint64(stride))
		t.emit(vm.OpSAdd)
		t.emit(sLdOp)
		t.sVstore(rt2)

		if inst.WB == 1 {
			emitWriteback()
		}
	}
	return nil
}

// trStackLoadReg 翻译 LDR (register offset) — 栈模式
// addr = Rn + (shift ? Rm << size : Rm)
func (t *Translator) trStackLoadReg(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	s := (inst.Raw >> 12) & 1
	size := (inst.Raw >> 30) & 3
	shift := uint32(0)
	if s == 1 {
		shift = size
	}

	// addr = Rn + (Rm << shift)
	t.sVload(rn)
	t.sVload(rm)
	if shift > 0 {
		t.sPushImm32(shift)
		t.emit(vm.OpSShl)
	}
	t.emit(vm.OpSAdd) // addr on stack

	op := Op(inst.Op)
	var sLdOp byte
	switch op {
	case LDRB_REG:
		sLdOp = vm.OpSLd8
	case LDRH_REG:
		sLdOp = vm.OpSLd16
	default:
		if inst.SF {
			sLdOp = vm.OpSLd64
		} else {
			sLdOp = vm.OpSLd32
		}
	}

	t.emit(sLdOp)
	t.sVstore(rd)
	return nil
}

// trStackStoreReg 翻译 STR (register offset) — 栈模式
func (t *Translator) trStackStoreReg(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rd, err := t.mapReg(inst.Rd) // Rt source
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	s := (inst.Raw >> 12) & 1
	size := (inst.Raw >> 30) & 3
	shift := uint32(0)
	if s == 1 {
		shift = size
	}

	// addr = Rn + (Rm << shift)
	t.sVload(rn)
	t.sVload(rm)
	if shift > 0 {
		t.sPushImm32(shift)
		t.emit(vm.OpSShl)
	}
	t.emit(vm.OpSAdd) // addr on stack

	op := Op(inst.Op)
	var sStOp byte
	switch op {
	case STRB_REG:
		sStOp = vm.OpSSt8
	case STRH_REG:
		sStOp = vm.OpSSt16
	default:
		if inst.SF {
			sStOp = vm.OpSSt64
		} else {
			sStOp = vm.OpSSt32
		}
	}

	t.pushRegOrZero(inst.Rd, rd)
	t.emit(sStOp)
	return nil
}

// trStackBitLogicalNot 翻译 BIC/ORN/EON — 栈模式
// Rd = Rn OP NOT(shift(Rm))
// vmStackOp: OpSAnd → BIC, OpSOr → ORN, OpSXor → EON
func (t *Translator) trStackBitLogicalNot(inst vm.Instruction, sOp byte, setFlags bool) error {
	rd, rn, rm, err := t.mapReg3(inst)
	if err != nil {
		return err
	}

	// push Rn
	t.pushRegOrZero(inst.Rn, rn)

	// push shift(Rm) then NOT
	t.pushRegOrZero(inst.Rm, rm)
	if inst.Shift != 0 {
		t.emitShiftOnStack(inst.ShiftType, uint32(inst.Shift), inst.SF)
	}
	t.emit(vm.OpSNot) // NOT(shift(Rm))

	// Rd = Rn OP NOT(shift(Rm))
	t.emit(sOp)

	if setFlags {
		t.sDup()
		t.sPushImm32(0)
		t.emit(vm.OpSCmp)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// trStackEON 翻译 EON — 栈模式
// EON = Rd = Rn XOR NOT(shift(Rm))
func (t *Translator) trStackEON(inst vm.Instruction) error {
	return t.trStackBitLogicalNot(inst, vm.OpSXor, false)
}

// trStackAddSubExt 翻译 ADD/SUB (extended register) — 栈模式
// Rd = Rn op extend(Rm, shift)
func (t *Translator) trStackAddSubExt(inst vm.Instruction, sOp byte, setFlags bool) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	// push Rn
	t.sVload(rn)

	// push extend(Rm)
	t.pushRegOrZero(inst.Rm, rm)
	option := inst.ShiftType
	switch option {
	case 0: // UXTB
		t.sPushImm32(0xFF)
		t.emit(vm.OpSAnd)
	case 1: // UXTH
		t.sPushImm32(0xFFFF)
		t.emit(vm.OpSAnd)
	case 2: // UXTW
		t.emit(vm.OpSTrunc32)
	case 3: // UXTX — no-op
	case 4: // SXTB: SHL 56, ASR 56
		t.sPushImm32(56)
		t.emit(vm.OpSShl)
		t.sPushImm32(56)
		t.emit(vm.OpSAsr)
	case 5: // SXTH: SHL 48, ASR 48
		t.sPushImm32(48)
		t.emit(vm.OpSShl)
		t.sPushImm32(48)
		t.emit(vm.OpSAsr)
	case 6: // SXTW: SHL 32, ASR 32
		t.emit(vm.OpSSext32)
	case 7: // SXTX — no-op
	}

	// 额外左移
	if inst.Shift > 0 {
		t.sPushImm32(uint32(inst.Shift))
		t.emit(vm.OpSShl)
	}

	// Rn op extend(Rm)
	t.emit(sOp)

	if setFlags {
		t.sDup()
		t.sPushImm32(0)
		t.emit(vm.OpSCmp)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}

	if inst.Rd == vm.REG_XZR {
		t.sDrop()
	} else {
		t.sVstore(rd)
	}
	return nil
}

// abs64 返回 int64 绝对值
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// regToStackOp 将 register-based opcode 映射到 stack-based opcode
func regToStackOp(regOp byte) byte {
	switch regOp {
	case vm.OpAdd:
		return vm.OpSAdd
	case vm.OpSub:
		return vm.OpSSub
	case vm.OpMul:
		return vm.OpSMul
	case vm.OpXor:
		return vm.OpSXor
	case vm.OpAnd:
		return vm.OpSAnd
	case vm.OpOr:
		return vm.OpSOr
	case vm.OpShl:
		return vm.OpSShl
	case vm.OpShr:
		return vm.OpSShr
	case vm.OpAsr:
		return vm.OpSAsr
	case vm.OpRor:
		return vm.OpSRor
	case vm.OpUmulh:
		return vm.OpSUmulh
	default:
		return 0
	}
}

// immToStackOp 将 imm-based opcode 映射到 stack-based opcode
func immToStackOp(immOp byte) byte {
	switch immOp {
	case vm.OpAddImm:
		return vm.OpSAdd
	case vm.OpSubImm:
		return vm.OpSSub
	case vm.OpMulImm:
		return vm.OpSMul
	case vm.OpXorImm:
		return vm.OpSXor
	case vm.OpAndImm:
		return vm.OpSAnd
	case vm.OpOrImm:
		return vm.OpSOr
	case vm.OpShlImm:
		return vm.OpSShl
	case vm.OpShrImm:
		return vm.OpSShr
	case vm.OpAsrImm:
		return vm.OpSAsr
	default:
		return 0
	}
}

// ============================================================
// 以下是从旧 register-based 翻译器迁移到栈模式的函数
// 完成后可删除 pickTemp/pickTemp2 及 tr_loadstore.go / tr_alu.go / tr_bitfield.go 中的旧版本
// ============================================================

// trStackLoadRegSigned 翻译 LDRSB/LDRSH/LDRSW (register offset) — 栈模式
// addr = Rn + (Rm << shift), load, sign-extend
func (t *Translator) trStackLoadRegSigned(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}

	s := (inst.Raw >> 12) & 1
	size := (inst.Raw >> 30) & 3
	shift := uint32(0)
	if s == 1 {
		shift = size
	}

	// addr = Rn + (Rm << shift) on stack
	t.sVload(rn)
	t.sVload(rm)
	if shift > 0 {
		t.sPushImm32(shift)
		t.emit(vm.OpSShl)
	}
	t.emit(vm.OpSAdd)

	// load
	op := Op(inst.Op)
	var sLdOp byte
	var sextBits uint32
	switch op {
	case LDRSB_REG:
		sLdOp = vm.OpSLd8
		sextBits = 56
	case LDRSH_REG:
		sLdOp = vm.OpSLd16
		sextBits = 48
	case LDRSW_REG:
		sLdOp = vm.OpSLd32
		sextBits = 32
	default:
		sLdOp = vm.OpSLd64
		sextBits = 0
	}
	t.emit(sLdOp)

	// sign-extend: SHL sextBits, ASR sextBits
	if sextBits > 0 {
		t.sPushImm32(sextBits)
		t.emit(vm.OpSShl)
		t.sPushImm32(sextBits)
		t.emit(vm.OpSAsr)
	}

	t.sVstore(rd)
	return nil
}

// trStackLdadd 翻译 LDADD — 原子加 (单线程简化) — 栈模式
// 语义: old = Mem[Rn]; Mem[Rn] = old + Rs; Rt = old
func (t *Translator) trStackLdadd(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rt, err := t.mapReg(inst.Rd) // Rt: receives old value
	if err != nil {
		return err
	}
	rs, err := t.mapReg(inst.Rm) // Rs: addend
	if err != nil {
		return err
	}

	var sLdOp, sStOp byte
	if inst.Shift <= 4 {
		sLdOp = vm.OpSLd32
		sStOp = vm.OpSSt32
	} else {
		sLdOp = vm.OpSLd64
		sStOp = vm.OpSSt64
	}

	// SSt pops addr(top), val(second) → Mem[addr] = val
	// SLd pops addr(top) → pushes Mem[addr]

	// 1) load old value
	t.sVload(rn)  // push addr
	t.emit(sLdOp) // pop addr, push old = Mem[addr]
	// stack: [old]

	// 2) store old → Rt
	t.emit(vm.OpSDup) // dup old
	t.sVstore(rt)     // Rt = old
	// stack: [old]

	// 3) compute new = old + Rs
	t.sVload(rs)      // push Rs
	t.emit(vm.OpSAdd) // new = old + Rs
	// stack: [new]

	// 4) store new → Mem[Rn]
	t.sVload(rn)  // push addr
	t.emit(sStOp) // Mem[addr] = new, pops both
	// stack: []

	return nil
}

// trStackCas 翻译 CAS — 比较并交换 (单线程简化) — 栈模式
// 语义: old = Mem[Rn]; if old == Xs then Mem[Rn] = Xt; Xs = old
// 单线程: 总是成功, 简化为: old=[Rn]; [Rn]=Xt; Rs=old
func (t *Translator) trStackCas(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rt, err := t.mapReg(inst.Rd) // Rt: new value to store
	if err != nil {
		return err
	}
	rs, err := t.mapReg(inst.Rm) // Rs: compare value, receives old
	if err != nil {
		return err
	}

	var sLdOp, sStOp byte
	if inst.Shift <= 4 {
		sLdOp = vm.OpSLd32
		sStOp = vm.OpSSt32
	} else {
		sLdOp = vm.OpSLd64
		sStOp = vm.OpSSt64
	}

	// Step 1: old = [Rn]
	t.sVload(rn)
	t.emit(sLdOp) // old on stack

	// Step 2: store Rt → [Rn]
	t.sVload(rt)  // push new value
	t.sVload(rn)  // push addr
	t.emit(sStOp) // Mem[addr] = new

	// Step 3: Rs = old (still on stack from step 1)
	t.sVstore(rs)

	return nil
}

// trStackLdpsw 翻译 LDPSW — Load pair of signed words — 栈模式
// 加载两个 32-bit 值并 sign-extend 到 64-bit
func (t *Translator) trStackLdpsw(inst vm.Instruction) error {
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rt1, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rt2, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}
	const stride = int64(4)

	emitWriteback := func(imm int64) {
		if imm >= 0 {
			t.sVload(rn)
			t.sPushImm(uint64(imm))
			t.emit(vm.OpSAdd)
			t.sVstore(rn)
		} else {
			t.sVload(rn)
			t.sPushImm(uint64(-imm))
			t.emit(vm.OpSSub)
			t.sVstore(rn)
		}
	}

	sextW := func(reg byte) {
		// sign-extend 32→64: SHL 32, ASR 32
		t.sVload(reg)
		t.sPushImm32(32)
		t.emit(vm.OpSShl)
		t.sPushImm32(32)
		t.emit(vm.OpSAsr)
		t.sVstore(reg)
	}

	if inst.WB == 3 { // pre-index
		emitWriteback(inst.Imm)
		// load [Rn+0]
		t.sVload(rn)
		t.emit(vm.OpSLd32)
		t.sVstore(rt1)
		// load [Rn+4]
		t.sVload(rn)
		t.sPushImm(uint64(stride))
		t.emit(vm.OpSAdd)
		t.emit(vm.OpSLd32)
		t.sVstore(rt2)
	} else {
		loadImm := inst.Imm
		if inst.WB == 1 {
			loadImm = 0
		}
		// load [Rn+loadImm] — 栈模式不需要 pickTemp, 即使 rt1==rn 也安全
		// 因为 VLOAD(rn) 在栈上复制了值，后续 VSTORE(rt1) 不影响栈上的地址
		t.sVload(rn)
		if loadImm != 0 {
			t.sPushImm(uint64(loadImm))
			t.emit(vm.OpSAdd)
		}
		t.emit(vm.OpSDup) // dup addr for second load
		t.emit(vm.OpSLd32)
		t.sVstore(rt1)
		// [addr still on stack] + stride
		t.sPushImm(uint64(stride))
		t.emit(vm.OpSAdd)
		t.emit(vm.OpSLd32)
		t.sVstore(rt2)

		if inst.WB == 1 {
			emitWriteback(inst.Imm)
		}
	}

	// Sign-extend both 32→64
	sextW(rt1)
	sextW(rt2)
	return nil
}

// trStackSMADDL 翻译 SMADDL/SMSUBL — 栈模式
// SMADDL: Xd = Xa + SEXT(Wn) * SEXT(Wm)
// SMSUBL: Xd = Xa - SEXT(Wn) * SEXT(Wm)
func (t *Translator) trStackSMADDL(inst vm.Instruction, isSub bool) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}
	raIdx := int((inst.Raw >> 10) & 0x1F)
	if raIdx == 31 {
		raIdx = vm.REG_XZR
	}
	ra, err := t.mapReg(raIdx)
	if err != nil {
		return err
	}

	// Push Ra (or 0 if XZR)
	t.pushRegOrZero(raIdx, ra)

	// SEXT(Wn): SHL 32, ASR 32
	t.sVload(rn)
	t.sPushImm32(32)
	t.emit(vm.OpSShl)
	t.sPushImm32(32)
	t.emit(vm.OpSAsr)

	// SEXT(Wm): SHL 32, ASR 32
	t.sVload(rm)
	t.sPushImm32(32)
	t.emit(vm.OpSShl)
	t.sPushImm32(32)
	t.emit(vm.OpSAsr)

	// multiply
	t.emit(vm.OpSMul)

	// Ra +/- product
	if isSub {
		// stack: [Ra, product] → Ra - product
		// SSub pops b(top), a(second), pushes a-b
		t.emit(vm.OpSSub)
	} else {
		t.emit(vm.OpSAdd)
	}

	t.sVstore(rd)
	return nil
}

// trStackUMADDL 翻译 UMADDL/UMSUBL — 栈模式
// UMADDL: Xd = Xa + ZEXT(Wn) * ZEXT(Wm)
// UMSUBL: Xd = Xa - ZEXT(Wn) * ZEXT(Wm)
func (t *Translator) trStackUMADDL(inst vm.Instruction, isSub bool) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}
	raIdx := int((inst.Raw >> 10) & 0x1F)
	if raIdx == 31 {
		raIdx = vm.REG_XZR
	}
	ra, err := t.mapReg(raIdx)
	if err != nil {
		return err
	}

	// Push Ra (or 0 if XZR)
	t.pushRegOrZero(raIdx, ra)

	// ZEXT(Wn): trunc32 on stack
	t.sVload(rn)
	t.emit(vm.OpSTrunc32)

	// ZEXT(Wm): trunc32 on stack
	t.sVload(rm)
	t.emit(vm.OpSTrunc32)

	// multiply
	t.emit(vm.OpSMul)

	// Ra +/- product
	if isSub {
		t.emit(vm.OpSSub)
	} else {
		t.emit(vm.OpSAdd)
	}

	t.sVstore(rd)
	return nil
}

// trStackBFM 翻译 BFM Xd, Xn, #immr, #imms — 位域移动 — 栈模式
// BFI alias:   imms < immr → dst_lsb = regsize-immr, width = imms+1
// BFXIL alias: imms >= immr → dst_lsb = 0, width = imms-immr+1
func (t *Translator) trStackBFM(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}

	immr := uint32(inst.Imm)
	imms := uint32(inst.Shift)
	regsize := uint32(64)
	if !inst.SF {
		regsize = 32
	}

	var width, srcLSB, dstLSB uint32
	if imms >= immr {
		width = imms - immr + 1
		srcLSB = immr
		dstLSB = 0
	} else {
		width = imms + 1
		srcLSB = 0
		dstLSB = regsize - immr
	}

	mask := uint64((1 << width) - 1)

	// --- 栈操作: extracted = (Rn >> srcLSB) & mask ---
	t.sVload(rn)
	if srcLSB > 0 {
		t.sPushImm32(srcLSB)
		t.emit(vm.OpSShr)
	}
	// & mask
	t.sPushImm(mask)
	t.emit(vm.OpSAnd)

	// << dstLSB
	if dstLSB > 0 {
		t.sPushImm32(dstLSB)
		t.emit(vm.OpSShl)
	}
	// stack: [extracted_shifted]

	// --- Rd = (Rd & ~(mask << dstLSB)) | extracted_shifted ---
	clearMask := ^(mask << dstLSB)
	if !inst.SF {
		clearMask &= 0xFFFFFFFF
	}
	t.sVload(rd)
	t.sPushImm(clearMask)
	t.emit(vm.OpSAnd)

	// OR with extracted
	t.emit(vm.OpSOr)

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}
	t.sVstore(rd)
	return nil
}

// trStackEXTR 翻译 EXTR Xd, Xn, Xm, #lsb — 位域提取 — 栈模式
// ROR alias: Rn == Rm → rotate right
// General:   result = (Rm >> lsb) | (Rn << (regSize-lsb))
func (t *Translator) trStackEXTR(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	rm, err := t.mapReg(inst.Rm)
	if err != nil {
		return err
	}
	lsb := uint32(inst.Imm)
	regSize := uint32(32)
	if inst.SF {
		regSize = 64
	}

	if inst.Rn == inst.Rm {
		// ROR alias: 栈模式
		t.sVload(rn)
		t.sPushImm32(lsb)
		t.sPushImm32(regSize)
		t.emit(vm.OpSRor)
	} else {
		// General EXTR: (Rm >> lsb) | (Rn << (regSize-lsb))
		// Part 1: Rm >> lsb
		t.sVload(rm)
		t.sPushImm32(lsb)
		t.emit(vm.OpSShr)

		// Part 2: Rn << (regSize-lsb)
		t.sVload(rn)
		t.sPushImm32(regSize - lsb)
		t.emit(vm.OpSShl)

		// OR them
		t.emit(vm.OpSOr)
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}
	t.sVstore(rd)
	return nil
}

// trStackUBFM 翻译 UBFM — 栈模式
// 覆盖所有 case: LSR, LSL, UXTB, UXTH, UBFX, UBFIZ
func (t *Translator) trStackUBFM(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}
	rn, err := t.mapReg(inst.Rn)
	if err != nil {
		return err
	}
	immr := uint32(inst.Imm)
	imms := uint32(inst.Shift)

	regSize := uint32(32)
	if inst.SF {
		regSize = 64
	}

	switch {
	case imms == regSize-1:
		// LSR
		t.sVload(rn)
		t.sPushImm32(immr)
		t.emit(vm.OpSShr)
	case imms+1 == immr:
		// LSL
		t.sVload(rn)
		t.sPushImm32(regSize - immr)
		t.emit(vm.OpSShl)
	case imms == 7 && immr == 0:
		// UXTB
		t.sVload(rn)
		t.sPushImm(0xFF)
		t.emit(vm.OpSAnd)
	case imms == 15 && immr == 0:
		// UXTH
		t.sVload(rn)
		t.sPushImm(0xFFFF)
		t.emit(vm.OpSAnd)
	default:
		if imms >= immr {
			// UBFX: (Rn >> immr) & mask
			width := imms - immr + 1
			t.sVload(rn)
			t.sPushImm32(immr)
			t.emit(vm.OpSShr)
			mask := uint64((1 << width) - 1)
			t.sPushImm(mask)
			t.emit(vm.OpSAnd)
		} else {
			// UBFIZ: (Rn & mask) << shift
			width := imms + 1
			shift := regSize - immr
			mask := uint64((1 << width) - 1)
			t.sVload(rn)
			t.sPushImm(mask)
			t.emit(vm.OpSAnd)
			t.sPushImm32(shift)
			t.emit(vm.OpSShl)
		}
	}

	if !inst.SF {
		t.emit(vm.OpSTrunc32)
	}
	t.sVstore(rd)
	return nil
}

// trStackLdrLiteral 翻译 LDR literal (PC-relative) — 栈模式
// ARM64: LDR Xt/Wt, [PC + imm19*4]
func (t *Translator) trStackLdrLiteral(inst vm.Instruction) error {
	rd, err := t.mapReg(inst.Rd)
	if err != nil {
		return err
	}

	absAddr := uint64(inst.Imm)

	// push absolute address on stack
	t.sPushImm(absAddr)

	op := Op(inst.Op)
	switch {
	case op == LDR_LIT && inst.SF:
		// LDR Xt, [PC+imm] — 64-bit load
		t.emit(vm.OpSLd64)
	case op == LDR_LIT && !inst.SF:
		// LDR Wt, [PC+imm] — 32-bit load
		t.emit(vm.OpSLd32)
		t.emit(vm.OpSTrunc32)
	default:
		// LDRSW literal: load 32-bit, sign-extend to 64-bit
		t.emit(vm.OpSLd32)
		t.sPushImm32(32)
		t.emit(vm.OpSShl)
		t.sPushImm32(32)
		t.emit(vm.OpSAsr)
	}

	t.sVstore(rd)
	return nil
}
