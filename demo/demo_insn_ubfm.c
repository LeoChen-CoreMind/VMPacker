#include <stdio.h>
#include <stdint.h>

/*
 * demo_insn_ubfm.c — UBFM width>=32 测试
 *
 * 测试 UBFX 提取位域宽度 >= 32 的情况
 * UBFX Xd, Xn, #lsb, #width  =>  UBFM Xd, Xn, #lsb, #(lsb+width-1)
 *
 * 测试1: UBFX X0, X0, #0, #33  (提取低33位)
 *   UBFM X0, X0, #0, #32  (immr=0, imms=32)
 *   输入: 0x3FFFFFFFF (34位) → 输出: 0x1FFFFFFFF (低33位)
 *
 * 测试2: UBFX X0, X0, #16, #33 (从bit16开始提取33位)
 *   UBFM X0, X0, #16, #48  (immr=16, imms=48)
 *   输入: 0x1FFFFFFFFFFFF (49位) → 输出: 0x1FFFFFFFF (33位)
 */

__attribute__((noinline)) int64_t check_ubfm(void) {
    uint64_t v1 = 0x3FFFFFFFFULL;  /* 低34位全1 */
    uint64_t r1 = 0;

    /* UBFX x0, x0, #0, #33 → 提取低33位 → 0x1FFFFFFFF */
    __asm__ volatile(
        "ubfx %[out], %[in], #0, #33\n"
        : [out] "=r"(r1)
        : [in] "r"(v1)
    );
    __asm__ volatile("nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop;");

    uint64_t v2 = 0x1FFFFFFFFFFFFULL; /* 低49位全1 */
    uint64_t r2 = 0;

    /* UBFX x0, x0, #16, #33 → 从bit16提取33位 → 0x1FFFFFFFF */
    __asm__ volatile(
        "ubfx %[out], %[in], #16, #33\n"
        : [out] "=r"(r2)
        : [in] "r"(v2)
    );
    __asm__ volatile("nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop;");

    if (r1 == 0x1FFFFFFFFULL && r2 == 0x1FFFFFFFFULL) return 1;
    printf("r1=%llx, r2=%llx\n", r1, r2);
    return 0;
}

__attribute__((noinline)) int64_t check_ubfm2(void) {
    uint64_t v1 = 0x3FFFFFFFFFFFFULL; /* 低50位全1，但第33位是0？ */
    uint64_t r1 = 0;

    /* UBFX x0, x0, #0, #33 → 提取低33位 → 0x1FFFFFFFF */
    __asm__ volatile(
        "ubfx %[out], %[in], #16, #33\n"
        : [out] "=r"(r1)
        : [in] "r"(v1)
    );
    __asm__ volatile("nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop;");


    if (r1 == 0x1FFFFFFFFULL) return 1;
    printf("r1=%llx\n", r1);
    return 0;
}


__attribute__((noinline)) int64_t check_ubfm3(void) {
    // 设置一个测试值，让低位和高位容易区分
    // 使用 0xFEDCBA9876543210 这样每个字节都不同
    uint64_t v1 = 0xFEDCBA9876543210ULL;
    uint64_t r1 = 0;

    /* UBFIZ x0, x1, #16, #8
     * 提取 x1 的低 8 位 (0x10)，左移 16 位
     * 结果应该是 0x0000000000100000
     */
    __asm__ volatile(
        "ubfiz %[out], %[in], #16, #8\n"
        : [out] "=r"(r1)
        : [in] "r"(v1)
    );
    __asm__ volatile("nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop; nop;");

    // 期望结果: 低8位是0x10，左移16位后变成 0x100000
    if (r1 == 0x100000ULL) return 1;
    printf("r1=%llx\n", r1);
    return 0;
}


int main(void) {
    int64_t v = check_ubfm();
    if (v == 1) { 
        printf("PASS:UBFM\n");
    } else {
        printf("FAIL:UBFM r=%ld\n", v);
    }
    v = check_ubfm2();
    if (v == 1) { 
        printf("PASS:UBFM2\n");
    } else {
        printf("FAIL:UBFM2 r=%ld\n", v);
    }
    v = check_ubfm3();
    if (v == 1) { 
        printf("PASS:UBFM3\n");
    } else {
        printf("FAIL:UBFM3 r=%ld\n", v);
    }
    return 1;
}