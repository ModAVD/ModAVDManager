//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <LocalAuthentication/LocalAuthentication.h>
#import <Security/Security.h>
#include <stdint.h>
#include <string.h>

// Queries are always noninteractive: CLI and background manager requests must
// return an error when Keychain needs consent, rather than opening a modal UI.
int32_t modavd_wrapping_key(const char *service, const char *account,
                           unsigned char *key, int create) {
    @autoreleasepool {
        NSString *serviceString = [NSString stringWithUTF8String:service];
        NSString *accountString = [NSString stringWithUTF8String:account];
        if (!serviceString || !accountString) return errSecParam;
        LAContext *context = [[LAContext alloc] init];
        context.interactionNotAllowed = YES;
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        NSMutableDictionary *query = [@{
            (__bridge id)kSecClass: (__bridge id)kSecClassGenericPassword,
            (__bridge id)kSecAttrService: serviceString,
            (__bridge id)kSecAttrAccount: accountString,
            (__bridge id)kSecAttrSynchronizable: @NO,
            (__bridge id)kSecUseAuthenticationContext: context,
            // The file-based login Keychain also needs this explicit flag;
            // LAContext alone covers the data-protection Keychain backend.
            (__bridge id)kSecUseAuthenticationUI: (__bridge id)kSecUseAuthenticationUIFail,
            (__bridge id)kSecReturnData: @YES,
            (__bridge id)kSecMatchLimit: (__bridge id)kSecMatchLimitOne,
        } mutableCopy];
#pragma clang diagnostic pop
        CFTypeRef result = NULL;
        BOOL created = NO;
        OSStatus status = SecItemCopyMatching((__bridge CFDictionaryRef)query, &result);
        if (status == errSecItemNotFound && create) {
            status = SecRandomCopyBytes(kSecRandomDefault, 32, key);
            if (status == errSecSuccess) {
                [query removeObjectForKey:(__bridge id)kSecReturnData];
                [query removeObjectForKey:(__bridge id)kSecMatchLimit];
                query[(__bridge id)kSecValueData] = [NSData dataWithBytes:key length:32];
                status = SecItemAdd((__bridge CFDictionaryRef)query, NULL);
                created = status == errSecSuccess;
                if (status == errSecDuplicateItem) {
                    // Another manager won the creation race. Read its key;
                    // never replace it or publish data encrypted with our key.
                    [query removeObjectForKey:(__bridge id)kSecValueData];
                    query[(__bridge id)kSecReturnData] = @YES;
                    query[(__bridge id)kSecMatchLimit] = (__bridge id)kSecMatchLimitOne;
                    status = SecItemCopyMatching((__bridge CFDictionaryRef)query, &result);
                }
            }
        }
        if (result) {
            if (status == errSecSuccess && CFGetTypeID(result) == CFDataGetTypeID()
                    && CFDataGetLength((CFDataRef)result) == 32) {
                memcpy(key, CFDataGetBytePtr((CFDataRef)result), 32);
            } else if (status == errSecSuccess) {
                status = errSecDecode;
            }
            CFRelease(result);
        } else if (status == errSecSuccess && !created) {
            status = errSecDecode;
        }
        [query release];
        [context release];
        return status;
    }
}
